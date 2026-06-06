// Command bu-kasir adalah scaffold satu-file layanan backend yang
// mengintegrasikan sistem internal dengan payment gateway Pakasir
// menggunakan pakasir-go-sdk.
//
// Scaffold ini sengaja menyimpan order di memory (bukan database) agar mudah
// dijalankan untuk demo/uji coba. Untuk produksi, ganti orderStore dengan
// implementasi yang didukung database dan tambahkan autentikasi pada endpoint
// internal. Lihat README.md untuk PRD, blueprint, dan panduan lengkap.
//
// Tiga fitur yang disorot:
//   - Mode Sandbox & Payment Simulator (PAKASIR_MODE, /simulate, guard IsSandbox)
//   - Fee By Merchant (customer_charge vs merchant_net)
//   - Webhook URL (/webhook/pakasir dengan validasi, idempotensi, routing sandbox)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/H0llyW00dzZ/pakasir-go-sdk/src/client"
	"github.com/H0llyW00dzZ/pakasir-go-sdk/src/constants"
	"github.com/H0llyW00dzZ/pakasir-go-sdk/src/i18n"
	"github.com/H0llyW00dzZ/pakasir-go-sdk/src/simulation"
	"github.com/H0llyW00dzZ/pakasir-go-sdk/src/transaction"
	"github.com/H0llyW00dzZ/pakasir-go-sdk/src/webhook"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("server berhenti dengan error", slog.Any("error", err))
		os.Exit(1)
	}
}

// run menyiapkan dependensi, menjalankan HTTP server, dan menunggu sinyal
// shutdown. Pola run() error memisahkan logika dari main agar mudah dites.
func run(logger *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("konfigurasi: %w", err)
	}

	// Inisialisasi client Pakasir dengan retry + timeout bawaan SDK.
	c := client.New(
		cfg.Project,
		cfg.APIKey,
		client.WithBaseURL(cfg.BaseURL),
		client.WithTimeout(10*time.Second),
		client.WithRetries(5),
		client.WithRetryWait(500*time.Millisecond, time.Minute),
		client.WithLanguage(i18n.Indonesian),
	)

	app := &server{
		cfg:    cfg,
		logger: logger,
		client: c,
		txn:    transaction.NewService(c),
		sim:    simulation.NewService(c),
		store:  newOrderStore(),
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown saat menerima SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("bu-kasir mulai",
			slog.String("addr", cfg.Addr),
			slog.String("mode", cfg.Mode),
			slog.Bool("fee_by_merchant", cfg.FeeByMerchant),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown diminta, menutup server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// config menampung seluruh konfigurasi yang dibaca dari environment.
type config struct {
	Addr          string
	Project       string
	APIKey        string
	BaseURL       string
	Mode          string // "sandbox" | "production"
	FeeByMerchant bool
}

func (c *config) sandbox() bool { return c.Mode == modeSandbox }

const (
	modeSandbox    = "sandbox"
	modeProduction = "production"
)

func loadConfig() (*config, error) {
	cfg := &config{
		Addr:          getenv("BUKASIR_ADDR", ":8080"),
		Project:       os.Getenv("PAKASIR_PROJECT"),
		APIKey:        os.Getenv("PAKASIR_API_KEY"),
		BaseURL:       getenv("PAKASIR_BASE_URL", "https://app.pakasir.com"),
		Mode:          strings.ToLower(getenv("PAKASIR_MODE", modeProduction)),
		FeeByMerchant: getenvBool("PAKASIR_FEE_BY_MERCHANT", false),
	}

	if cfg.Project == "" || cfg.APIKey == "" {
		return nil, errors.New("PAKASIR_PROJECT dan PAKASIR_API_KEY wajib diisi")
	}
	if cfg.Mode != modeSandbox && cfg.Mode != modeProduction {
		return nil, fmt.Errorf("PAKASIR_MODE tidak valid: %q (gunakan %q atau %q)", cfg.Mode, modeSandbox, modeProduction)
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

// order adalah representasi order internal beserta mapping ke transaksi Pakasir.
type order struct {
	OrderID        string                      `json:"order_id"`
	Amount         int64                       `json:"amount"`
	Fee            int64                       `json:"fee"`
	TotalPayment   int64                       `json:"total_payment"`
	CustomerCharge int64                       `json:"customer_charge"`
	MerchantNet    int64                       `json:"merchant_net"`
	PaymentMethod  constants.PaymentMethod     `json:"payment_method"`
	PaymentNumber  string                      `json:"payment_number"`
	Status         constants.TransactionStatus `json:"status"`
	IsSandbox      bool                        `json:"is_sandbox"`
	ExpiredAt      string                      `json:"expired_at,omitempty"`
}

// orderStore adalah penyimpanan order in-memory yang aman untuk concurrent.
// Ganti dengan implementasi berbasis database untuk produksi.
type orderStore struct {
	mu     sync.RWMutex
	orders map[string]*order
}

func newOrderStore() *orderStore {
	return &orderStore{orders: make(map[string]*order)}
}

func (s *orderStore) save(o *order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[o.OrderID] = o
}

func (s *orderStore) get(orderID string) (*order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[orderID]
	return o, ok
}

func (s *orderStore) update(orderID string, fn func(*order)) (*order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[orderID]
	if !ok {
		return nil, false
	}
	fn(o)
	return o, true
}

// server menyatukan dependensi yang dibutuhkan oleh seluruh handler.
type server struct {
	cfg    *config
	logger *slog.Logger
	client *client.Client
	txn    *transaction.Service
	sim    *simulation.Service
	store  *orderStore
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/payments", s.handleCreatePayment)
	mux.HandleFunc("GET /api/payments/{order_id}", s.handleGetPayment)
	mux.HandleFunc("POST /api/payments/{order_id}/cancel", s.handleCancelPayment)
	mux.HandleFunc("POST /api/payments/{order_id}/simulate", s.handleSimulatePayment)
	mux.HandleFunc("GET /api/payments/{order_id}/qr", s.handleQR)
	mux.HandleFunc("POST /webhook/pakasir", s.handleWebhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "mode": s.cfg.Mode})
	})
	return mux
}

// createPaymentRequest adalah body untuk POST /api/payments.
type createPaymentRequest struct {
	OrderID string                  `json:"order_id"`
	Amount  int64                   `json:"amount"`
	Method  constants.PaymentMethod `json:"method"`
}

func (s *server) handleCreatePayment(w http.ResponseWriter, r *http.Request) {
	var req createPaymentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "body tidak valid")
		return
	}
	if req.OrderID == "" || req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "order_id dan amount (>0) wajib diisi")
		return
	}
	method := req.Method
	if method == "" {
		method = constants.MethodQRIS
	}
	if !method.Valid() {
		writeError(w, http.StatusBadRequest, "payment method tidak dikenal")
		return
	}

	resp, err := s.txn.Create(r.Context(), method, &transaction.CreateRequest{
		OrderID: req.OrderID,
		Amount:  req.Amount,
	})
	if err != nil {
		s.logger.Error("gagal create transaksi", slog.Any("error", err), slog.String("order_id", req.OrderID))
		writeError(w, http.StatusBadGateway, "gagal membuat transaksi di Pakasir")
		return
	}

	p := resp.Payment
	customerCharge, merchantNet := s.feeSplit(p.Amount, p.Fee, p.TotalPayment)
	o := &order{
		OrderID:        p.OrderID,
		Amount:         p.Amount,
		Fee:            p.Fee,
		TotalPayment:   p.TotalPayment,
		CustomerCharge: customerCharge,
		MerchantNet:    merchantNet,
		PaymentMethod:  p.PaymentMethod,
		PaymentNumber:  p.PaymentNumber,
		Status:         constants.StatusPending,
		IsSandbox:      s.cfg.sandbox(),
		ExpiredAt:      p.ExpiredAt,
	}
	s.store.save(o)

	writeJSON(w, http.StatusCreated, o)
}

func (s *server) handleGetPayment(w http.ResponseWriter, r *http.Request) {
	orderID := r.PathValue("order_id")

	// Verifikasi status terkini langsung ke Pakasir untuk rekonsiliasi.
	o, ok := s.store.get(orderID)
	if !ok {
		writeError(w, http.StatusNotFound, "order tidak ditemukan")
		return
	}

	detail, err := s.txn.Detail(r.Context(), &transaction.DetailRequest{
		OrderID: orderID,
		Amount:  o.Amount,
	})
	if err != nil {
		// Detail gagal bukan fatal; kembalikan snapshot lokal.
		s.logger.Warn("gagal ambil detail, pakai snapshot lokal", slog.Any("error", err), slog.String("order_id", orderID))
		writeJSON(w, http.StatusOK, o)
		return
	}

	o, _ = s.store.update(orderID, func(cur *order) {
		cur.Status = detail.Transaction.Status
	})
	writeJSON(w, http.StatusOK, o)
}

func (s *server) handleCancelPayment(w http.ResponseWriter, r *http.Request) {
	orderID := r.PathValue("order_id")
	o, ok := s.store.get(orderID)
	if !ok {
		writeError(w, http.StatusNotFound, "order tidak ditemukan")
		return
	}

	if err := s.txn.Cancel(r.Context(), &transaction.CancelRequest{
		OrderID: orderID,
		Amount:  o.Amount,
	}); err != nil {
		s.logger.Error("gagal cancel transaksi", slog.Any("error", err), slog.String("order_id", orderID))
		writeError(w, http.StatusBadGateway, "gagal membatalkan transaksi")
		return
	}

	o, _ = s.store.update(orderID, func(cur *order) {
		cur.Status = constants.StatusCancelled
	})
	writeJSON(w, http.StatusOK, o)
}

// handleSimulatePayment hanya aktif saat mode sandbox.
func (s *server) handleSimulatePayment(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.sandbox() {
		writeError(w, http.StatusForbidden, "simulasi hanya tersedia di mode sandbox")
		return
	}
	orderID := r.PathValue("order_id")
	o, ok := s.store.get(orderID)
	if !ok {
		writeError(w, http.StatusNotFound, "order tidak ditemukan")
		return
	}

	if err := s.sim.Pay(r.Context(), &simulation.PayRequest{
		OrderID: orderID,
		Amount:  o.Amount,
	}); err != nil {
		s.logger.Error("gagal simulasi pembayaran", slog.Any("error", err), slog.String("order_id", orderID))
		writeError(w, http.StatusBadGateway, "gagal melakukan simulasi pembayaran")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"order_id": orderID,
		"message":  "simulasi dikirim; webhook akan dipanggil oleh Pakasir",
	})
}

func (s *server) handleQR(w http.ResponseWriter, r *http.Request) {
	orderID := r.PathValue("order_id")
	o, ok := s.store.get(orderID)
	if !ok {
		writeError(w, http.StatusNotFound, "order tidak ditemukan")
		return
	}
	if o.PaymentMethod != constants.MethodQRIS {
		writeError(w, http.StatusBadRequest, "QR hanya tersedia untuk metode QRIS")
		return
	}

	w.Header().Set("Content-Type", "image/png")
	if err := s.client.QR().Write(w, o.PaymentNumber); err != nil {
		s.logger.Error("gagal render QR", slog.Any("error", err), slog.String("order_id", orderID))
		// Header sudah terkirim; cukup log.
	}
}

// handleWebhook menerima notifikasi pembayaran dari Pakasir.
func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	event, err := webhook.ParseRequest(r, webhook.WithMaxBodySize(64<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "payload webhook tidak valid")
		return
	}
	if err := event.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "field webhook tidak valid")
		return
	}

	o, ok := s.store.get(event.OrderID)
	if !ok {
		writeError(w, http.StatusBadRequest, "order tidak ditemukan")
		return
	}
	// Cocokkan amount asli (bukan customer_charge) terhadap data internal.
	if event.Amount != o.Amount {
		writeError(w, http.StatusBadRequest, "amount webhook tidak cocok")
		return
	}

	// Routing mode: event sandbox tidak boleh memicu fulfillment nyata.
	if event.IsSandbox {
		s.store.update(event.OrderID, func(cur *order) { cur.Status = event.Status })
		s.logger.Info("webhook sandbox diterima (tanpa fulfillment)",
			slog.String("order_id", event.OrderID), slog.String("status", string(event.Status)))
		w.WriteHeader(http.StatusOK)
		return
	}

	// Idempotensi: webhook bisa terkirim lebih dari sekali.
	if o.Status == constants.StatusCompleted {
		w.WriteHeader(http.StatusOK)
		return
	}

	if event.Status == constants.StatusCompleted {
		// Disarankan: verifikasi ulang via Detail API sebelum fulfillment nyata.
		s.store.update(event.OrderID, func(cur *order) { cur.Status = constants.StatusCompleted })
		s.logger.Info("order dibayar", slog.String("order_id", event.OrderID))
		// TODO(produksi): trigger fulfillment (kirim barang/email) secara async.
	} else {
		s.store.update(event.OrderID, func(cur *order) { cur.Status = event.Status })
	}

	w.WriteHeader(http.StatusOK)
}

// feeSplit menghitung nominal yang ditampilkan ke pembeli (customerCharge) dan
// nominal bersih yang diterima merchant (merchantNet) berdasarkan setting
// Fee By Merchant.
func (s *server) feeSplit(amount, fee, totalPayment int64) (customerCharge, merchantNet int64) {
	if s.cfg.FeeByMerchant {
		// Merchant menanggung fee: pembeli bayar amount, merchant terima amount - fee.
		return amount, amount - fee
	}
	// Default: pembeli menanggung fee.
	return totalPayment, amount
}

// --- helper HTTP ---

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
