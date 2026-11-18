# DroidBeam — SMS Gateway Android

Ubah HP Android jadi SMS gateway. Aplikasimu cukup memanggil REST API server, lalu server meneruskan pesan ke HP yang mengirim SMS lewat SIM-nya. SMS yang masuk ke HP juga diteruskan balik ke server lewat webhook.

```
Aplikasi kamu ──REST──▶ Server Go ◀──long-poll── HP Android (DroidBeam) ──SMS──▶ penerima
                          │   ▲                       │
                 webhook ◀┘   └── status / SMS masuk / baterai / info device
```

- `server/`: server Go dengan SQLite (satu binary, tanpa dependensi C).
- `android/`: app Kotlin + Jetpack Compose (minSdk 26 / Android 8.0).
- `scripts/get-lan-ip.sh`: ambil IP LAN server lewat domain publik.

---

## 1. Menjalankan server

```bash
cd server
go build -o droidbeam-server .
DROIDBEAM_API_KEY=$(openssl rand -hex 24) ./droidbeam-server
```

Simpan API key itu. Key ini dipakai aplikasimu dan saat mendaftarkan HP.

| Variabel | Default | Keterangan |
|---|---|---|
| `DROIDBEAM_API_KEY` | (wajib) | Rahasia minimal 16 karakter |
| `DROIDBEAM_ADDR` | `:8080` | Alamat listen |
| `DROIDBEAM_DB` | `droidbeam.db` | File SQLite |
| `DROIDBEAM_WEBHOOK_URL` | – | URL tujuan semua event webhook |
| `DROIDBEAM_WEBHOOK_SECRET` | – | Kalau diisi, body ditandatangani HMAC-SHA256 |
| `DROIDBEAM_BATTERY_THRESHOLDS` | `20,80` | Level baterai (%) yang memicu webhook saat dilewati |
| `DROIDBEAM_LAN_URL` | (deteksi otomatis) | Alamat LAN yang diumumkan ke HP/script, mis. `http://192.168.1.10:8080`. Wajib diisi kalau server jalan di Docker |
| `DROIDBEAM_OFFLINE_AFTER` | `2m` | HP dianggap offline kalau tidak menghubungi server selama ini |
| `DROIDBEAM_SEND_TIMEOUT` | `10m` | Pesan yang diambil HP tapi tidak dilaporkan hasilnya ditandai `failed` |

### Docker

```bash
cp .env.example .env      # isi DROIDBEAM_API_KEY dan DROIDBEAM_LAN_URL
docker compose up -d
docker compose logs -f
```

- Image-nya kecil (±13 MB): binary Go statis di atas `distroless`, berjalan sebagai user non-root.
- Database disimpan di volume `droidbeam-data` (`/data/droidbeam.db`), jadi tidak hilang saat container dibuat ulang.
- **Isi `DROIDBEAM_LAN_URL`** dengan IP LAN host Docker, misalnya `http://192.168.1.10:8080`. Dari dalam container, server tidak bisa melihat IP LAN host.
- Container punya healthcheck ke `/healthz` dan otomatis restart (`restart: unless-stopped`).
- Zona waktu default `Asia/Jakarta`. Zona ini dipakai untuk hitungan "hari ini" di dashboard; ubah lewat `TZ`.

Tanpa compose:
```bash
docker build -t droidbeam-server ./server
docker run -d --name droidbeam -p 8080:8080 -v droidbeam-data:/data \
  -e DROIDBEAM_API_KEY=... -e DROIDBEAM_LAN_URL=http://192.168.1.10:8080 droidbeam-server
```

### LAN + domain publik (satu server, dua alamat)

Jalankan server di jaringan lokal, lalu ekspos ke internet lewat domain, misalnya dengan Cloudflare Tunnel, reverse proxy di VPS, atau port forwarding + HTTPS. Hasilnya, server yang sama bisa diakses lewat:

- **LAN**: `http://192.168.1.10:8080` (cepat, tetap jalan walau internet putus)
- **Publik**: `https://sms.domainmu.com`

Saat start, server mencetak alamat LAN yang terdeteksi:
```
LAN address(es) advertised to devices: [http://192.168.1.10:8080]
```

> Gunakan **HTTPS** untuk alamat publik. HTTP polos hanya untuk LAN.

### Dashboard web

Buka `http://<server>:8080/` di browser, lalu masuk dengan API key. Dashboard ikut tertanam di binary server, tanpa CDN atau internet, jadi tetap jalan di LAN. Isinya:

- **Ringkasan hari ini**: perangkat online, pesan terkirim, pesan gagal, dan SMS masuk.
- **Kartu perangkat**: status online/offline, baterai (level, mengisi, suhu), status sinyal tiap SIM, jalur (LAN/cloud), IP, dan waktu terakhir terlihat.
- **Form kirim SMS**: perkiraan jumlah SMS, plus pilihan perangkat dan SIM.
- **Daftar pesan keluar** dengan filter status, dan **daftar SMS masuk**.
- Diperbarui otomatis tiap 5 detik (dijeda saat tab tidak aktif).

API key hanya disimpan di browser (sessionStorage, atau localStorage kalau "Ingat di browser ini" dicentang). Jangan buka dashboard lewat HTTP polos dari internet; gunakan HTTPS untuk alamat publik.

---

## 2. Memasang app Android

**Cara termudah:** unduh APK dari halaman [Releases](https://github.com/rayenking/DroidBeam/releases/latest), lalu pasang di HP (izinkan "instal dari sumber tidak dikenal").

Atau build sendiri:
```bash
cd android
./gradlew assembleDebug
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

Bisa juga buka folder `android/` di Android Studio lalu Run.

Di HP:
1. **Hubungkan ke server**: isi *Alamat cloud / domain*, *API key*, dan nama perangkat. *Alamat LAN* boleh dikosongkan kalau "Cari IP LAN otomatis lewat domain" aktif. Lalu tekan **Daftarkan perangkat**.
2. **Berikan izin** SMS dan notifikasi.
3. **Kecualikan dari optimasi baterai**. Ini penting agar gateway tidak dimatikan sistem.
4. Nyalakan switch **Gateway**.

### Agar tetap hidup (background / foreground / startup)

| Mekanisme | Fungsi |
|---|---|
| Foreground service (`remoteMessaging`) + notifikasi tetap | Tetap jalan saat app ditutup |
| `START_STICKY` | Android menyalakan ulang service kalau dimatikan |
| `BootReceiver` | Menyala otomatis setelah HP restart atau app di-update |
| `WatchdogWorker` (WorkManager, tiap 15 menit + 30 detik setelah service mati) | Menghidupkan lagi kalau dibunuh task killer OEM |
| Partial wake lock + Wi-Fi lock | Polling tetap jalan saat layar mati / Doze |
| Network callback | Langsung reconnect begitu jaringan kembali |
| Status & SMS masuk via WorkManager | Tidak hilang walau sedang offline; dikirim ulang otomatis |
| Semua error di loop ditangkap | Satu respons aneh tidak membuat app crash |

**Khusus Xiaomi / Oppo / Vivo / Realme / Samsung**: aktifkan **Autostart** dan set baterai app ke **Tanpa batasan** (tombol *Buka pengaturan app*). Ini **wajib**. Tanpa Autostart, HyperOS/MIUI memblokir app menyala sendiri setelah HP restart atau app di-update (terlihat di logcat: `process is not permitted to auto start`). Untuk HP yang khusus dijadikan gateway, biarkan selalu terhubung ke charger.

**Batas SMS Android**: secara default Android meminta konfirmasi kalau satu app mengirim lebih dari ±30 SMS dalam 30 menit. Untuk HP gateway, naikkan batasnya lewat adb:
```bash
adb shell settings put global sms_outgoing_check_max_count 10000
```
```bash
adb shell settings put global sms_outgoing_check_interval_ms 60000
```

### Fallback LAN → cloud

- App memakai alamat **LAN** dulu. Kalau gagal tersambung, request otomatis diulang lewat **domain**.
- Selama berada di cloud, app mengecek LAN tiap 60 detik (atau segera setelah jaringan berganti) dan kembali ke LAN kalau bisa.
- Dengan *Cari IP LAN otomatis*, app menanyakan IP LAN server lewat domain (`/api/v1/device/server-info`), lalu memverifikasi lewat `server_id` bahwa alamat itu memang server yang sama. Jadi kalau IP LAN server berubah (DHCP), HP ikut menyesuaikan sendiri.

---

## 3. API

Semua endpoint klien memakai header `X-API-Key: <API key>`.

### Kirim SMS
```bash
curl -X POST https://sms.domainmu.com/api/v1/messages \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{"to":"+6281234567890","body":"Halo dari DroidBeam"}'
```
Field opsional:
- `"device_id": 1`: paksa lewat HP tertentu.
- `"sim": 2`: paksa lewat SIM 2. Kalau tidak diisi, app memakai SIM default SMS; kalau SIM itu **tanpa layanan**, app otomatis pindah ke SIM yang ada sinyal.

Respons `202`:
```json
{"id":1,"to":"+6281234567890","body":"Halo dari DroidBeam","status":"queued","device_id":null,"error":null,
 "created_at":"…","updated_at":"…","sent_at":null,"delivered_at":null,"sim":null,"sim_used":null}
```
`sim_used` diisi HP dengan nomor slot SIM yang benar-benar dipakai.
Status pesan: `queued` → `sending` → `sent` → `delivered`, atau `failed` (dengan `error`).

| Method | Path | Keterangan |
|---|---|---|
| `POST` | `/api/v1/messages` | Kirim SMS (maks 1600 karakter, otomatis dipecah multipart) |
| `GET` | `/api/v1/messages/{id}` | Status satu pesan |
| `GET` | `/api/v1/messages?status=&limit=&before=` | Daftar pesan (terbaru dulu) |
| `GET` | `/api/v1/inbox?limit=&before=` | SMS masuk |
| `GET` | `/api/v1/devices` | Semua HP, termasuk info dan baterai |
| `GET` | `/api/v1/devices/{id}` | Info satu HP, termasuk baterai |
| `POST` | `/api/v1/devices` | Daftarkan HP (biasanya dilakukan oleh app) |
| `GET` | `/api/v1/server-info` | `server_id` + `lan_urls` (IP LAN server) |
| `GET` | `/api/v1/stats` | Jumlah pesan per status (semua & hari ini) + SMS masuk hari ini |
| `GET` | `/healthz` | Health check (tanpa auth) |

### Info device + baterai
```bash
curl -H "X-API-Key: $KEY" https://sms.domainmu.com/api/v1/devices/1
```
```json
{
  "id": 1, "name": "HP Kantor", "online": true,
  "created_at": "2026-11-18T11:27:21.845Z", "last_seen_at": "2026-11-18T11:30:02.112Z",
  "info": {
    "manufacturer": "Samsung", "model": "Galaxy A15", "android_version": "14", "sdk": 34,
    "app_version": "0.1.0", "operator": "Telkomsel", "network": "wifi",
    "connection": "primary", "local_ip": "192.168.1.23",
    "sims": [
      {"slot": 1, "carrier": "TELKOMSEL", "in_service": false, "default_sms": true},
      {"slot": 2, "carrier": "XL", "in_service": true, "default_sms": false}
    ]
  },
  "battery": {
    "level": 100, "status": "full", "plugged": "ac", "temperature": 30.2,
    "health": "good", "updated_at": "2026-11-18T11:30:02.112Z"
  }
}
```
- `info.connection`: `primary` (LAN) atau `fallback` (cloud).
- `info.local_ip`: IP HP di jaringannya.
- `info.sims`: SIM yang terpasang, apakah ada sinyal, dan mana yang default untuk SMS.
- `battery.status`: `charging`, `discharging`, `full`, `not_charging`, atau `unknown`.
- `battery.plugged`: `ac`, `usb`, `wireless`, `dock`, atau `none`.

HP mengirim data ini setiap kali level atau status pengisian berubah, dan tiap 5 menit sebagai heartbeat.

### Ambil IP LAN server lewat domain
```bash
DROIDBEAM_URL=https://sms.domainmu.com DROIDBEAM_API_KEY=$KEY ./scripts/get-lan-ip.sh
```
Contoh output: `http://192.168.1.10:8080`.

Tambahkan `--check` agar hanya alamat yang terjangkau dari mesin tersebut (dan terverifikasi sebagai server yang sama) yang dicetak. Script ini setara dengan:
```bash
curl -H "X-API-Key: $KEY" https://sms.domainmu.com/api/v1/server-info
# {"lan_urls":["http://192.168.1.10:8080"],"server_id":"3be7bd7b…"}
```

---

## 4. Webhook

Semua event dikirim sebagai `POST` JSON ke `DROIDBEAM_WEBHOOK_URL`, dengan percobaan ulang hingga 4 kali.

```json
{ "event": "battery.threshold", "data": { … }, "sent_at": "2026-11-18T11:30:02Z" }
```

| Event | Kapan | `data` |
|---|---|---|
| `message.status` | Status pesan berubah | objek pesan |
| `sms.received` | SMS masuk ke HP | `{id, device_id, from, body, received_at, created_at, sim}` |
| `battery.threshold` | Level melewati ambang (default 20% dan 80%) | `device`, `battery`, `threshold`, `direction` (`down`/`up`) |
| `battery.full` | Baterai penuh | `device`, `battery` |
| `battery.charger_connected` | Charger dicolok | `device`, `battery` |
| `battery.charger_disconnected` | Charger dicabut | `device`, `battery` |
| `device.online` | HP tersambung lagi | `device` |
| `device.offline` | HP tidak menghubungi server > 2 menit | `device` |

Contoh: "baterai turun ke 20%" berarti `event == "battery.threshold" && data.threshold == 20 && data.direction == "down"`, dan "baterai naik ke 80%" berarti `threshold == 80 && direction == "up"`. Untuk menambah ambang lain: `DROIDBEAM_BATTERY_THRESHOLDS=10,20,50,80`.

Header yang menyertai setiap request:
- `X-DroidBeam-Event: <event>`
- `X-DroidBeam-Signature: sha256=<hex>` (kalau secret diisi), yaitu HMAC-SHA256 dari raw body dengan `DROIDBEAM_WEBHOOK_SECRET`.

Contoh verifikasi (Node.js):
```js
const sig = 'sha256=' + crypto.createHmac('sha256', SECRET).update(rawBody).digest('hex');
const ok = crypto.timingSafeEqual(Buffer.from(sig), Buffer.from(req.headers['x-droidbeam-signature']));
```

---

## 5. Pengembangan

```bash
cd server
go test ./...
```
```bash
cd android
./gradlew assembleDebug lintDebug
```

### Build APK release

Buat `android/keystore.properties` (sudah di-ignore git) yang menunjuk ke keystore-mu:
```properties
storeFile=/path/ke/droidbeam-release.jks
storePassword=...
keyAlias=droidbeam
keyPassword=...
```
lalu jalankan `./gradlew assembleRelease`. Tanpa file itu, hasilnya APK release yang belum ditandatangani.

> APK release dan APK debug ditandatangani dengan kunci berbeda. Untuk pindah dari debug ke release, uninstall app lama dulu, lalu daftarkan ulang perangkatnya.

### Catatan
- App ini tidak bisa dipublikasikan di Google Play (izin SMS dibatasi); pasang dengan sideload.
- Token perangkat disimpan sebagai hash SHA-256 di server dan hanya ditampilkan sekali saat pendaftaran.
- Dual SIM butuh izin *status telepon* (`READ_PHONE_STATE`) untuk melihat SIM mana yang ada sinyal.
- Database dari versi lama otomatis di-upgrade saat server start.
