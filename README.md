# Bu Kasir – Integrasi Pakasir (Go & Pakasir Go SDK)

Bu Kasir adalah layanan backend yang mengintegrasikan sistem internal dengan payment gateway [Pakasir](https://pakasir.com) menggunakan bahasa Go dan library tidak resmi [`pakasir-go-sdk`](https://github.com/H0llyW00dzZ/pakasir-go-sdk). Layanan ini menangani pembuatan transaksi, pembatalan, pengecekan status, dan penerimaan webhook pembayaran dari Pakasir. [1][2]

---

## 1. PRD (Product Requirements Document)

### 1.1 Tujuan Produk

Bu Kasir menyediakan satu titik integrasi (single integration point) antara aplikasi internal (POS, mobile app, web app) dengan Pakasir, dengan fokus pada:

- Pembuatan transaksi pembayaran (QRIS dan Virtual Account).
- Penerimaan dan validasi webhook pembayaran.
- Penyediaan API internal untuk membaca status pembayaran.
- Menjaga keamanan API key Pakasir dan menghindari ekspos ke frontend. [1][2]

### 1.2 Ruang Lingkup (Scope) Versi 1

**Fungsional utama:**

- Buat transaksi pembayaran ke Pakasir:
  - Minimal metode QRIS (`qris`), dan dapat diperluas ke VA bank lain sesuai kebutuhan. [1][2]
- Batalkan transaksi pembayaran (cancel) ketika order dibatalkan di sistem internal. [1][2]
- Ambil detail status transaksi melalui Transaction Detail API Pakasir (untuk rekonsiliasi atau verifikasi tambahan). [1]
- Terima webhook pembayaran dari Pakasir dan update status order internal berdasarkan status transaksi yang dikirim. [1][2]
- (Opsional) Generate gambar QR code (PNG) dari payment number untuk ditampilkan ke kasir/pelanggan. [2]

**Di luar scope v1 (contoh):**

- Fitur refund atau partial refund (tidak tercantum di dokumentasi resmi Pakasir).
- Laporan settlement harian otomatis.
- Dashboard UI untuk monitoring (fokus v1 hanya backend service).

### 1.3 Persyaratan Non-Fungsional

- **Teknologi:**
  - Bahasa Go minimal versi 1.26 (sesuai requirement `pakasir-go-sdk`). [2]
- **Reliability:**
  - Kegagalan sementara (misal HTTP 429, 502, 503, 504, dan error jaringan) harus otomatis di-retry dengan exponential backoff dan jitter (menggunakan mekanisme bawaan `pakasir-go-sdk`). [2]
- **Security:**
  - API key Pakasir disimpan di environment variables, bukan dalam source code.
  - Endpoint Transaction Detail (`GET /api/transactiondetail`) Pakasir hanya dipanggil dari backend karena API key dikirim sebagai parameter query, sesuai spesifikasi Pakasir. [1][2]
  - Webhook tidak boleh langsung memicu fulfillment tanpa:
    - Validasi `order_id` dan `amount` terhadap data internal.
    - (Opsional) Verifikasi tambahan dengan memanggil Transaction Detail API. [1][2]
- **Observability (minimal):**
  - Logging structured minimal untuk request ke Pakasir dan error respons (tanpa menulis API key ke log).
  - Metric dasar: jumlah transaksi sukses/gagal (bisa ditambahkan kemudian).

### 1.4 Ringkasan Requirement

| ID | Jenis | Requirement | Detail Singkat |
|---|---|---|---|
| FR-1 | Fungsional | Create Payment | Sistem dapat membuat transaksi Pakasir (QRIS, optional VA) dari order internal |
| FR-2 | Fungsional | Cancel Payment | Sistem dapat membatalkan transaksi Pakasir untuk order yang dibatalkan |
| FR-3 | Fungsional | Payment Status | Sistem dapat membaca status transaksi melalui webhook dan Detail API |
| FR-4 | Fungsional | QR Code | Sistem dapat menyediakan QR code pembayaran dalam bentuk PNG |
| NFR-1 | Non-Fungsional | Retry | Kegagalan sementara di-request ke Pakasir di-retry otomatis dengan backoff |
| SEC-1 | Security | API Key Management | API key hanya ada di backend, tidak pernah dikirim ke client |
| SEC-2 | Security | Webhook Validation | Webhook diverifikasi terhadap data internal dan (opsional) via Detail API |

---

## 2. Blueprint Arsitektur

### 2.1 Gambaran Tingkat Tinggi

Secara garis besar, arsitektur integrasi Bu Kasir dengan Pakasir adalah sebagai berikut:

```text
[Client App / POS / Web / Mobile]
               |
               v
           [Bu Kasir API]
               |
               |-- menggunakan --> [pakasir-go-sdk]
               |                        |
               |                        v
               |                 [Pakasir REST API]
               |
               |<-- Webhook --- [Pakasir Webhook]
               v
    [Database / Order & Payment Store]
```

- Client hanya berkomunikasi dengan Bu Kasir (bukan langsung ke Pakasir).
- Bu Kasir menggunakan `pakasir-go-sdk` untuk memanggil API Pakasir:
  - Create, Cancel, Detail, Simulation. [2]
- Pakasir mengirim webhook ke endpoint Bu Kasir ketika status pembayaran berubah (misal jadi `completed`). [1][2]
- Database menyimpan order internal serta mapping ke transaksi Pakasir (order_id, payment_number, status, dll.).

### 2.2 Data Dictionary (Field Utama)

Berdasarkan dokumentasi resmi Pakasir dan struktur response yang ditunjukkan untuk Transaction Create dan Detail, berikut field-field kunci yang digunakan: [1][2]

**Entity: Payment (Pakasir)**

| Field | Tipe | Sumber | Deskripsi |
|---|---|---|---|
| project | string | Pakasir | Slug project di Pakasir |
| order_id | string | Pakasir | ID order yang dikirim merchant (sistem internal) |
| amount | int | Pakasir | Nominal transaksi dalam satuan terkecil (mis. rupiah) |
| fee | int | Pakasir | Biaya admin transaksi (jika ada) [1] |
| total_payment | int | Pakasir | Jumlah yang harus dibayar pelanggan (amount + fee) [1] |
| payment_method | string | Pakasir / SDK constants | Metode pembayaran: `qris`, `bni_va`, dll. [1][2] |
| payment_number | string | Pakasir | Nomor VA atau string pembayaran QRIS [1] |
| expired_at | string (timestamp, zona waktu) | Pakasir | Waktu kedaluwarsa pembayaran [1] |

**Entity: Transaction (Pakasir)**

| Field | Tipe | Sumber | Deskripsi |
|---|---|---|---|
| project | string | Pakasir | Slug project |
| order_id | string | Pakasir | ID order internal |
| amount | int | Pakasir | Nominal transaksi |
| status | string | Pakasir / SDK constants | Status transaksi: `completed`, `pending`, `expired`, `cancelled`, dsb. [1][2] |
| payment_method | string | Pakasir | Metode pembayaran |
| completed_at | string (timestamp) | Pakasir | Waktu transaksi diselesaikan (jika berhasil) [1] |

**Mapping ke Tabel Internal (Contoh: `orders`)**

| Tabel Internal | Field | Maps to |
|---|---|---|
| orders | external_order_id | Payment.order_id |
| orders | external_project | Payment.project |
| orders | external_payment_method | Payment.payment_method |
| orders | external_payment_number | Payment.payment_number |
| orders | external_status | Transaction.status |
| orders | external_amount | Payment.amount |
| orders | external_total_payment | Payment.total_payment |
| orders | external_completed_at | Transaction.completed_at |

### 2.3 Penggunaan Objek dan Fungsi SDK

Bu Kasir menggunakan fungsi-fungsi inti berikut dari `pakasir-go-sdk`: [2]

- **Client & konfigurasi:**
  - `client.New(projectSlug, apiKey, ...options)`  
    Membuat klien HTTP ke Pakasir dengan dukungan:
    - Timeout (`WithTimeout`)
    - Retry dengan exponential backoff (`WithRetries`, `WithRetryWait`)
    - Bahasa error (EN/ID) (`WithLanguage`)
    - Batas ukuran response (`WithMaxResponseSize`)
    - Pengaturan QR code (`WithQRCodeOptions`)

- **Layanan transaksi:**
  - `transaction.NewService(c)`  
    Membuat service transaksi dari client.
  - `Create(ctx, method, *transaction.CreateRequest)`  
    Memanggil `POST /api/transactioncreate/{method}` untuk membuat transaksi baru. [1][2]
  - `Cancel(ctx, *transaction.CancelRequest)`  
    Memanggil `POST /api/transactioncancel` untuk membatalkan transaksi. [1][2]
  - `Detail(ctx, *transaction.DetailRequest)`  
    Memanggil `GET /api/transactiondetail` untuk mengambil status transaksi. [1][2]

- **Layanan simulasi:**
  - `simulation.NewService(c)` dan `Pay(ctx, *simulation.PayRequest)`  
    Digunakan untuk sandbox/testing pembayaran. [2]

- **Webhook:**
  - `webhook.Parse(r io.Reader)`  
  - `webhook.ParseRequest(r *http.Request)`  
  - `webhook.ParseBytes(b []byte)`  
    Untuk parsing payload webhook menjadi `webhook.Event` serta memberikan sentinel error terstruktur. [2]

- **QR Code:**
  - `c.QR().Encode(paymentNumber)`  
  - `c.QR().Write(w, paymentNumber)`  
  - `c.QR().WriteFile(filename, paymentNumber)`  
    Untuk menghasilkan QR code PNG dari string pembayaran QRIS. [2]

- **URL Builder:**
  - `url.Build(...)`  
    Untuk membangun URL redirect pembayaran Pakasir (misalkan jika ingin mengarahkan user ke halaman pembayaran di browser). [2][1]

---

## 3. Panduan Pengguna (User Guide) – Step by Step

Bagian ini ditujukan untuk developer internal yang ingin mengintegrasikan aplikasi mereka dengan Bu Kasir.

### 3.1 Prasyarat

- Go versi 1.26 atau lebih baru. [2]
- Akses dashboard Pakasir untuk mendapatkan:
  - **Project slug** (misal: `my-project`).
  - **API key** server-side. [1]
- Akses ke database untuk menyimpan order dan transaksi.

### 3.2 Instalasi Dependensi

Pasang SDK Go Pakasir tidak resmi:

```bash
go get github.com/H0llyW00dzZ/pakasir-go-sdk
```

[2]

### 3.3 Konfigurasi Environment

Set environment variables (contoh):

```bash
export PAKASIR_PROJECT=my-project-slug
export PAKASIR_API_KEY=your_api_key_here
export PAKASIR_BASE_URL=https://pakasir.com
```

- `PAKASIR_PROJECT` dan `PAKASIR_API_KEY` didapat dari dashboard Pakasir. [1]
- `PAKASIR_BASE_URL` dapat dibiarkan default jika tidak diperlukan override khusus.

### 3.4 Inisialisasi Client di Bu Kasir

Contoh inisialisasi di kode Go:

```go
package pakasirclient

import (
    "os"
    "time"

    "github.com/H0llyW00dzZ/pakasir-go-sdk/src/client"
    "github.com/H0llyW00dzZ/pakasir-go-sdk/src/i18n"
)

func NewClient() *client.Client {
    return client.New(
        os.Getenv("PAKASIR_PROJECT"),
        os.Getenv("PAKASIR_API_KEY"),
        client.WithBaseURL(os.Getenv("PAKASIR_BASE_URL")),      // opsional
        client.WithTimeout(10*time.Second),
        client.WithRetries(5),
        client.WithRetryWait(500*time.Millisecond, time.Minute),
        client.WithLanguage(i18n.Indonesian),                   // pesan error Bahasa Indonesia
    )
}
```

[2]

Di tempat lain, buat service transaksi:

```go
import (
    "github.com/H0llyW00dzZ/pakasir-go-sdk/src/transaction"
    "github.com/H0llyW00dzZ/pakasir-go-sdk/src/constants"
)

var txnService *transaction.Service

func InitServices() {
    c := NewClient()
    txnService = transaction.NewService(c)
}
```

[2]

### 3.5 Membuat Transaksi Pembayaran

Misal Bu Kasir expose endpoint internal: `POST /api/payments`.

**Flow:**

1. Client mengirim `order_id` dan `amount` ke Bu Kasir.
2. Bu Kasir menyimpan order di database (status awal `pending`).
3. Bu Kasir memanggil Pakasir via SDK:

```go
resp, err := txnService.Create(ctx, constants.MethodQRIS, &transaction.CreateRequest{
    OrderID: orderID,
    Amount:  amount,
})
if err != nil {
    // handle error (log, return 500, dsb.)
    return
}
```

[1][2]

4. Dari `resp.Payment`, simpan ke database:
   - `project`
   - `order_id`
   - `amount`
   - `fee`
   - `total_payment`
   - `payment_method`
   - `payment_number`
   - `expired_at` [1][2]

5. Return ke client salah satu dari:
   - String QR (payment_number) untuk di-render QR di sisi client.
   - URL redirect yang dibuat dari helper `url.Build(...)` bila ingin mengarahkan user ke halaman pembayaran Pakasir. [1][2]

### 3.6 Menyajikan QR Code Ke Client

Jika ingin server Bu Kasir menghasilkan PNG:

```go
import (
    "net/http"

    "github.com/H0llyW00dzZ/pakasir-go-sdk/src/helper/qr"
)

// Misalkan c adalah *client.Client yang sudah diinisialisasi
func HandleQR(w http.ResponseWriter, r *http.Request) {
    // Ambil payment_number dari database berdasarkan order_id
    paymentNumber := "..." // hasil lookup

    w.Header().Set("Content-Type", "image/png")
    err := c.QR().Write(w, paymentNumber)
    if err != nil {
        http.Error(w, "gagal membuat QR", http.StatusInternalServerError)
        return
    }
}
```

[2]

### 3.7 Menangani Webhook Pakasir

Pakasir akan mengirim webhook ketika status transaksi berubah (misal payment completed). [1]

Expose endpoint Bu Kasir: `POST /webhook/pakasir`.

```go
import (
    "log"
    "net/http"

    "github.com/H0llyW00dzZ/pakasir-go-sdk/src/webhook"
    "github.com/H0llyW00dzZ/pakasir-go-sdk/src/constants"
)

func PakasirWebhookHandler(w http.ResponseWriter, r *http.Request) {
    event, err := webhook.ParseRequest(r)
    if err != nil {
        http.Error(w, "request tidak valid", http.StatusBadRequest)
        return
    }

    // 1. Lookup order internal berdasarkan event.OrderID
    order, err := findOrderByExternalOrderID(event.OrderID)
    if err != nil {
        http.Error(w, "order tidak ditemukan", http.StatusBadRequest)
        return
    }

    // 2. Validasi amount
    if event.Amount != order.ExternalAmount {
        http.Error(w, "amount tidak cocok", http.StatusBadRequest)
        return
    }

    // 3. (Opsional) verifikasi tambahan dengan Detail API
    // detail, err := txnService.Detail(ctx, ...)
    // ...

    // 4. Jika status completed, update order internal
    if event.Status == constants.StatusCompleted {
        err = markOrderAsPaid(order.ID)
        if err != nil {
            log.Println("gagal update order:", err)
            http.Error(w, "gagal update order", http.StatusInternalServerError)
            return
        }
    }

    w.WriteHeader(http.StatusOK)
}
```

[1][2]

Poin penting:

- Jangan pernah memakai webhook tanpa validasi `order_id` dan `amount`.
- Webhook sandbox tidak boleh memicu fulfillment sungguhan.

### 3.8 Membatalkan Transaksi

Jika order dibatalkan di sistem internal sebelum pembayaran selesai, Bu Kasir dapat memanggil Transaction Cancel. [1][2]

Misal endpoint internal: `POST /api/payments/{order_id}/cancel`.

```go
import "github.com/H0llyW00dzZ/pakasir-go-sdk/src/transaction"

func CancelPayment(ctx context.Context, orderID string) error {
    _, err := txnService.Cancel(ctx, &transaction.CancelRequest{
        OrderID: orderID,
    })
    if err != nil {
        return err
    }

    return markOrderAsCancelled(orderID)
}
```

[2]

### 3.9 Membaca Detail Transaksi

Untuk rekonsiliasi atau pengecekan manual status, gunakan Transaction Detail. Per dokumentasi resmi, API key dikirim sebagai parameter query. [1]

Contoh wrapper:

```go
import "github.com/H0llyW00dzZ/pakasir-go-sdk/src/transaction"

func GetPaymentDetail(ctx context.Context, orderID string, amount int) (*transaction.DetailResponse, error) {
    resp, err := txnService.Detail(ctx, &transaction.DetailRequest{
        OrderID: orderID,
        Amount:  amount,
    })
    if err != nil {
        return nil, err
    }
    return resp, nil
}
```

[1][2]

Catatan keamanan:

- Endpoint ini hanya boleh dipanggil dari backend karena API key berada di query string, yang berpotensi tertulis di access log jika tidak disanitasi. [1][2]

### 3.10 Mode Sandbox & Payment Simulator

Sandbox adalah mode uji coba. Transaksi yang terjadi di mode ini **tidak** masuk ke saldo utama, dan QRIS/Virtual Account yang dibuat **tidak bisa** di-scan atau ditransfer. Production adalah mode produksi: transaksi masuk ke saldo utama dan diproses oleh sistem.

**Penting:** `pakasir-go-sdk` tidak memiliki "mode switch". Mode (Sandbox vs Production) ditentukan oleh **project Pakasir** yang dipakai (pasangan `slug` + `api_key`). Oleh karena itu, Bu Kasir menyimpan konfigurasi mode sendiri untuk gating internal:

```bash
export PAKASIR_MODE=sandbox   # sandbox | production (default: production)
```

Gunakan slug + API key project Sandbox saat `PAKASIR_MODE=sandbox`, dan project Production saat `PAKASIR_MODE=production`.

Saat project masih di mode Sandbox, Anda dapat melakukan simulasi pembayaran untuk menguji webhook tanpa pembayaran nyata. SDK menyediakan `simulation.Service`: [2]

```go
import "github.com/H0llyW00dzZ/pakasir-go-sdk/src/simulation"

simService := simulation.NewService(c)

func SimulatePayment(ctx context.Context, orderID string, amount int64) error {
    // Hanya boleh dipanggil saat PAKASIR_MODE=sandbox.
    return simService.Pay(ctx, &simulation.PayRequest{
        OrderID: orderID,
        Amount:  amount,
    })
}
```

Endpoint internal yang disarankan: `POST /api/payments/{order_id}/simulate`.

- Endpoint ini **hanya aktif** saat `PAKASIR_MODE=sandbox`; di mode production kembalikan `403`/`404`.
- `simulation.Service.Pay` memanggil `POST /api/paymentsimulation` dan memicu webhook seolah pembayaran nyata selesai. [1][2]
- Simulasi hanya bekerja untuk transaksi berstatus `pending` di project Sandbox.

**Guardrail fulfillment:** webhook yang dihasilkan simulasi memiliki flag `Event.IsSandbox == true`. Jangan pernah memicu fulfillment nyata (kirim barang, potong stok permanen) untuk event sandbox — cukup update status untuk keperluan tes. Tandai juga order yang lahir di sandbox (mis. kolom `is_sandbox` pada tabel `orders`).

### 3.11 Fee By Merchant

Secara default, biaya transaksi (`fee`) dibebankan kepada **pembeli**. Jika fitur **Fee By Merchant** diaktifkan, biaya transaksi dibebankan kepada **Anda sebagai merchant**.

**Penting:** Fee By Merchant **bukan** parameter API atau field SDK. Ini adalah **setting di dashboard Pakasir (Edit Proyek)**. Efeknya hanya terlihat dari response `Transaction Create` melalui field `amount`, `fee`, dan `total_payment` (= `amount` + `fee`). [1]

Agar logika tampilan dan rekonsiliasi konsisten dengan setting dashboard, Bu Kasir menyimpan cerminan konfigurasi:

```bash
export PAKASIR_FEE_BY_MERCHANT=false   # true | false (samakan dengan setting dashboard)
```

Logika penentuan nominal:

| Fee By Merchant | Pembeli membayar | Merchant terima (net) | Yang ditampilkan ke pembeli |
|---|---|---|---|
| OFF (default) | `total_payment` (= amount + fee) | `amount` | `total_payment` |
| ON | `amount` | `amount - fee` | `amount` |

Dari response `Create`, selain menyimpan `amount`, `fee`, `total_payment`, hitung dan simpan turunan:

- `customer_charge` = (Fee By Merchant ON ? `amount` : `total_payment`) — nominal yang ditampilkan ke pembeli / POS.
- `merchant_net` = (Fee By Merchant ON ? `amount - fee` : `amount`) — nominal untuk laporan & rekonsiliasi.

**Catatan validasi:** webhook dan Transaction Detail dari Pakasir selalu mengirim `amount` (nominal asli), **bukan** `customer_charge`. Saat mencocokkan webhook dengan order internal, gunakan `amount` asli. Disarankan menambahkan log peringatan bila `fee`/`total_payment` dari Pakasir tidak konsisten dengan asumsi `PAKASIR_FEE_BY_MERCHANT` (deteksi drift setting dashboard).

### 3.12 Webhook URL (Konfigurasi & Pengetatan)

Webhook URL adalah URL endpoint di server Anda yang dipanggil oleh sistem Pakasir saat transaksi terbayar (`completed`). URL ini diisi melalui form **Edit Proyek** di dashboard Pakasir, contoh: `https://<domain-bukasir>/webhook/pakasir`. [1]

Penanganan webhook dasar sudah dijelaskan di §3.7. Berikut pengetatan yang disarankan untuk produksi:

```go
import (
    "github.com/H0llyW00dzZ/pakasir-go-sdk/src/webhook"
    "github.com/H0llyW00dzZ/pakasir-go-sdk/src/constants"
)

func PakasirWebhookHandler(w http.ResponseWriter, r *http.Request) {
    // 1. Batasi ukuran body (default SDK 1 MB; perketat sesuai kebutuhan).
    event, err := webhook.ParseRequest(r, webhook.WithMaxBodySize(64<<10))
    if err != nil {
        http.Error(w, "request tidak valid", http.StatusBadRequest)
        return
    }

    // 2. Sanity-check field webhook (order_id non-empty, amount > 0).
    if err := event.Validate(); err != nil {
        http.Error(w, "payload tidak valid", http.StatusBadRequest)
        return
    }

    // 3. Lookup + cocokkan order_id & amount (asli) terhadap data internal.
    order, err := findOrderByExternalOrderID(event.OrderID)
    if err != nil {
        http.Error(w, "order tidak ditemukan", http.StatusBadRequest)
        return
    }
    if event.Amount != order.ExternalAmount {
        http.Error(w, "amount tidak cocok", http.StatusBadRequest)
        return
    }

    // 4. Routing mode: jangan fulfillment nyata untuk event sandbox.
    if event.IsSandbox {
        _ = markOrderStatus(order.ID, event.Status) // update status saja
        w.WriteHeader(http.StatusOK)
        return
    }

    // 5. Idempotensi: lewati jika order sudah completed (webhook bisa terkirim >1x).
    if order.ExternalStatus == string(constants.StatusCompleted) {
        w.WriteHeader(http.StatusOK)
        return
    }

    // 6. (Disarankan) verifikasi ulang via Transaction Detail API sebelum fulfillment.
    //    detail, err := txnService.Detail(ctx, &transaction.DetailRequest{...})

    // 7. Update status & fulfillment.
    if event.Status == constants.StatusCompleted {
        if err := markOrderAsPaid(order.ID); err != nil {
            http.Error(w, "gagal update order", http.StatusInternalServerError)
            return
        }
    }

    // 8. Balas 200 cepat; proses berat (email, dsb.) sebaiknya async.
    w.WriteHeader(http.StatusOK)
}
```

Struktur payload webhook (dari docs): [1]

```json
{
  "amount": 22000,
  "order_id": "240910HDE7C9",
  "project": "depodomain",
  "status": "completed",
  "payment_method": "qris",
  "completed_at": "2024-09-10T08:07:02.819+07:00"
}
```

SDK juga membaca field `is_sandbox` ke `Event.IsSandbox`. Docs Pakasir menyarankan tetap melakukan pengecekan status yang lebih valid melalui Transaction Detail API, karena webhook saja tidak cukup untuk memicu fulfillment. [1]

### 3.13 Menjalankan Scaffold (`main.go`)

Repo ini menyertakan scaffold implementasi satu file ([`main.go`](main.go)) yang menerapkan ketiga fitur di atas. Scaffold menyimpan order di memory (bukan database) agar mudah dijalankan untuk demo; ganti `orderStore` dengan implementasi berbasis database dan tambahkan autentikasi pada endpoint internal sebelum dipakai produksi.

```bash
export PAKASIR_PROJECT=my-project-slug
export PAKASIR_API_KEY=your_api_key_here
export PAKASIR_MODE=sandbox            # sandbox | production (default: production)
export PAKASIR_FEE_BY_MERCHANT=false   # samakan dengan setting dashboard
# opsional: BUKASIR_ADDR (default :8080), PAKASIR_BASE_URL (default https://app.pakasir.com)

go run .
```

Endpoint yang diekspos:

| Method & Path | Fungsi |
|---|---|
| `POST /api/payments` | Buat transaksi (body: `order_id`, `amount`, opsional `method`) |
| `GET /api/payments/{order_id}` | Ambil status (verifikasi via Detail API) |
| `POST /api/payments/{order_id}/cancel` | Batalkan transaksi |
| `POST /api/payments/{order_id}/simulate` | Simulasi pembayaran (hanya mode `sandbox`) |
| `GET /api/payments/{order_id}/qr` | Render QR PNG (QRIS) |
| `POST /webhook/pakasir` | Terima webhook Pakasir |
| `GET /healthz` | Health check |

---

## 4. Catatan Keamanan

- Simpan API key Pakasir hanya di environment/server-side.
- Hindari logging URL lengkap yang mengandung query `api_key` (terutama untuk endpoint Detail).
- Lakukan rotasi API key secara berkala melalui dashboard Pakasir.
- Jangan expose endpoint Bu Kasir ke internet tanpa autentikasi dan proteksi standar (rate limiting, firewall, dsb.).
- Pastikan webhook URL tidak mudah ditebak, dan terproteksi dengan baik.

---

## 5. Lisensi

Sesuaikan bagian ini dengan lisensi yang kamu inginkan untuk proyek Bu Kasir.

```text
Copyright (c) 2026
All rights reserved.
```
