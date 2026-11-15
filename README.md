# RestreamCamera

Nhận 1 (hoặc vài) luồng camera thật — RTSP / HTTP / HLS / SRT / file — và nhân bản thành hàng trăm "camera ảo" độc lập (RTSP / HLS / WebRTC / SRT) để test tải VMS / NVR / AI pipeline.

- **Media engine:** [MediaMTX 1.21.3](https://mediamtx.org) (Control API v3, auth HTTP callback, on-demand source)
- **Control plane:** Go 1.25 + SQLite (thuần Go, không cần CGO) + UI nhúng sẵn trong binary (không cần Node/build frontend)
- **Deploy:** Docker Compose, `network_mode: host` cho production
- **Quy mô mục tiêu:** ~200 camera ảo trên một node, fan-out không transcode (chỉ 1 luồng ingest)

## Kiến trúc

```
Camera thật ──RTSP/HTTP──▶  src-<id8>  (MediaMTX pull 1 lần, on-demand)
                                │
                ┌───────────────┼───────────────┐
                ▼               ▼               ▼
          vcam-0001        vcam-0002  ...  vcam-0200      (RTSP/HLS/WHEP/SRT)
                │               │               │
             VMS/NVR ────── client đọc đồng thời (maxReaders, auth, allowlist)
```

- DB (SQLite) là **source of truth**. Reconciler chạy mỗi 5s: so sánh DB với `/v3/config/paths/list`, tự thêm/sửa/xoá path và cập nhật trạng thái runtime (readers, kbps).
- Mỗi camera ảo có **credential riêng** (user/password sinh tự động hoặc đặt tay), có thể gắn **IP allowlist**, `maxReaders`, `onDemand`, chế độ `proxy` hoặc `redirect`.
- Mọi read/publish đều được MediaMTX gọi về `POST /api/v1/mediamtx/auth` của control plane. Origin path (`src-*`) chỉ cho phép đọc từ loopback (trừ khi có camera ở mode `redirect`).
- Secret (URL camera thật, password camera ảo) mã hoá **AES-256-GCM**; master key lấy từ `RC_MASTER_KEY` / `RC_MASTER_KEY_FILE` hoặc tự sinh trong `data/master.key`.

## Cấu trúc repo

| Đường dẫn | Nội dung |
|---|---|
| `backend/cmd/restreamd` | Entrypoint: `serve`, `healthcheck`, `backup`, `version` |
| `backend/internal/api` | REST API, SSE monitor, load test, auth callback |
| `backend/internal/provision` | Reconciler MediaMTX ↔ SQLite |
| `backend/internal/store` | SQLite schema + CRUD (WAL, single writer) |
| `backend/internal/mediamtx` | Client Control API v3 (có phân trang) |
| `backend/internal/rtsp` | DESCRIBE client tối giản (probe + load test) |
| `backend/internal/webui/assets` | SPA thuần JS/CSS nhúng vào binary |
| `deploy/` | Compose, cấu hình MediaMTX, script backup/smoke test |
| `docs/PLAN.md` | Kế hoạch kỹ thuật đầy đủ (kiến trúc, API, sizing, roadmap) |

## Quick start (production, host network)

```bash
cd deploy
cp .env.example .env      # sửa RC_PUBLIC_HOST + RC_ADMIN_PASSWORD
docker compose up -d --build
docker compose ps
```

Mở UI: `http://<host>:8080` (mặc định `admin` / mật khẩu trong `.env`).

### Build & push image của riêng bạn

Image do project build chỉ có **một**: `${RC_IMAGE}:${RC_TAG}` (từ `backend/Dockerfile`). MediaMTX, golang, alpine đều là image kéo về.

Dùng **một tag cố định** (`latest`) và push chồng lên mỗi lần release — không tạo tag theo từng version:

```bash
cd deploy
docker compose -f docker-compose.yml build api
docker compose -f docker-compose.yml push api
```

Đặt trong `.env` để không phải truyền biến mỗi lần:

```bash
RC_IMAGE=unray/restreamcamera-api
RC_TAG=latest
```

`RC_VERSION` chỉ là version nhúng trong binary (`restreamd version`), không ảnh hưởng tag image. Máy khác chạy không cần source:

```yaml
services:
  api:
    image: unray/restreamcamera-api:latest
```

Các cổng cần mở trên host:

| Cổng | Giao thức | Dùng cho |
|---|---|---|
| 8080 | TCP | UI + REST API |
| 8554 | TCP + UDP | RTSP (UDP dùng dải ephemeral 32768–60999 của kernel) |
| 8888 | TCP | HLS |
| 8889 | TCP | WebRTC / WHEP |
| 8189 | UDP | WebRTC media |
| 8890 | UDP | SRT |
| 9997, 9998 | TCP | Control API + metrics (chỉ bind 127.0.0.1) |

> RTSP-over-UDP: MediaMTX cấp port UDP động cho mỗi session. Nếu firewall chặn, hoặc dùng Docker Desktop, hãy đổi `rtspTransports: [tcp]` trong `deploy/mediamtx.yml` và cấu hình client đọc bằng TCP.

Thư mục `deploy/data` chứa SQLite + master key. Trên Linux host:

```bash
sudo mkdir -p deploy/data && sudo chown -R 10001:10001 deploy/data
```

## Quick start (deploy, pull image từ Docker Hub)

Dùng khi deploy lên máy/VPS khác **không cần source code**, chỉ pull image đã build sẵn
(`unray/restreamcamera-api`) về và chạy. File compose: `deploy/docker-compose.deploy.yml`.

```bash
# trên máy đích: chỉ cần Docker + file deploy/ (compose, mediamtx.yml, .env.example)
cd deploy
cp .env.example .env
# sửa .env: bắt buộc RC_PUBLIC_HOST (IP LAN thật của máy, KHÔNG để localhost)
#            và RC_ADMIN_PASSWORD; RC_IMAGE/RC_TAG đã có default, chỉ đổi nếu
#            bạn push lên namespace Docker Hub khác.

# Linux: thư mục data phải thuộc UID 10001 (user trong image, không phải root)
sudo mkdir -p data && sudo chown -R 10001:10001 data

docker compose -f docker-compose.deploy.yml pull
docker compose -f docker-compose.deploy.yml up -d
docker compose -f docker-compose.deploy.yml ps
```

Mở UI: `http://<RC_PUBLIC_HOST>:8080` (đăng nhập bằng `RC_ADMIN_USER`/`RC_ADMIN_PASSWORD` trong `.env`).

**Cập nhật bản mới** (tag `latest` bị push chồng ở mỗi release, không cần đổi `.env`):

```bash
cd deploy
docker compose -f docker-compose.deploy.yml pull
docker compose -f docker-compose.deploy.yml up -d
```

**Vì sao web và link RTSP truy cập được từ mọi interface:**

- Cả hai container chạy `network_mode: host` → không bị giới hạn bởi NAT/port-mapping của Docker.
- `RC_LISTEN=0.0.0.0:8080` (đặt sẵn trong compose) → UI/API nghe trên **mọi** địa chỉ IP của máy, không chỉ `127.0.0.1`.
- `mediamtx.yml` khai báo `rtspAddress: :8554`, `hlsAddress: :8888`, `webrtcAddress: :8889`, `srtAddress: :8890` (không ghi IP cụ thể) → MediaMTX cũng nghe trên mọi interface.
- `RC_PUBLIC_HOST` trong `.env` là địa chỉ được **nhúng vào link** (`rtsp://...`, `http://...` cho HLS/WHEP) mà webapp trả về cho client — đây phải là IP LAN mà các máy khác (VMS, trình duyệt, ffplay) thực sự gõ vào để kết nối tới server, không phải `localhost`/`127.0.0.1`. Nếu máy có nhiều card mạng, chọn IP thuộc mạng mà client của bạn nằm trên đó; muốn phục vụ nhiều mạng cùng lúc, chạy thêm một `RC_PUBLIC_HOST` khác qua biến môi trường riêng hoặc dùng DNS/VIP trỏ về cùng máy.

Bảng cổng cần mở trên host và lưu ý RTSP-over-UDP: xem mục "Quick start (production, host network)" ở trên — áp dụng y hệt cho cách deploy này.

**Máy đã có sẵn một MediaMTX khác (hoặc một stack RestreamCamera khác) chạy ở cổng mặc định?**
Vì `network_mode: host`, hai container không thể cùng bind một cổng — bỏ comment và đổi
các biến `RC_HTTP_PORT` / `RC_RTSP_PORT` / `RC_HLS_PORT` / `RC_WEBRTC_PORT` /
`RC_WEBRTC_UDP_PORT` / `RC_SRT_PORT` / `RC_MEDIA_API_PORT` / `RC_MEDIA_METRICS_PORT`
trong `.env` (default = cổng chuẩn ở bảng trên) thành cổng còn trống. Không cần sửa
`docker-compose*.yml` hay `mediamtx.yml` — cả hai file compose đã đọc các biến này và tự
set lại `RC_LISTEN`, `RC_PUBLIC_*` cũng như override MediaMTX qua biến môi trường
`MTX_*`. Mỗi stack chạy song song cần: bộ cổng riêng, thư mục `data` riêng (copy `deploy/`
sang thư mục khác hoặc đổi `RC_DATA_DIR`/volume), và project name riêng
(`docker compose -p <ten-khac> -f docker-compose.deploy.yml up -d`, vì mặc định cả
hai file đều đặt `name: restreamcamera`).

## Quick start (dev, bridge network)

Dùng khi phát triển trên Windows/macOS (RTSP chỉ TCP, WebRTC advertise `127.0.0.1`):

```bash
cd deploy
docker compose -f docker-compose.dev.yml up -d --build
# UI: http://localhost:8080
```

Nếu cổng bị trùng, remap toàn bộ:

```bash
RC_DEV_HTTP_PORT=18080 RC_DEV_RTSP_PORT=18554 RC_DEV_HLS_PORT=18888 \
RC_DEV_WEBRTC_PORT=18889 RC_DEV_WEBRTC_UDP=18189 RC_DEV_SRT_PORT=18890 \
RC_DEV_API_MEDIA_PORT=19997 \
docker compose -f docker-compose.dev.yml up -d
```

## Sử dụng

1. **Sources** → `+ Thêm source`
   - `pull`: dán URL camera thật (`rtsp://user:pass@192.168.1.50:554/...`).
   - `publisher`: tạo path `src-xxxxxxxx` rồi đẩy stream vào, ví dụ:
     ```bash
     ffmpeg -re -i input.mp4 -c copy -f rtsp rtsp://<host>:8554/src-xxxxxxxx
     ```
2. **Cameras** → `+ Tạo camera ảo`: chọn source, prefix, số lượng (ví dụ 200), group, `maxReaders`, mode.
   Slug sinh ra dạng `vcam-0001`…; mật khẩu hiển thị **một lần** và có thể tải lại qua nút `CSV`.
3. Đọc thử:
   ```bash
   ffplay "rtsp://vcam-0001:<password>@<host>:8554/vcam-0001"
   # HLS
   ffplay "http://<host>:8888/vcam-0001/index.m3u8"
   ```
4. **Load test**: mô phỏng N client DESCRIBE đồng thời qua auth thật, xem p50/p95/fail theo từng lỗi.
5. **Monitor**: readers, ingress/egress, trạng thái từng camera (SSE, cập nhật 2s).

Trạng thái camera: `ready` (có media), `idle` (on-demand, chưa có client), `offline` (có client nhưng không có media), `disabled`, `error`.

## API (rút gọn)

```
POST   /api/v1/auth/login              {username,password} → {token}
GET    /api/v1/overview | /samples?limit= | /events?limit= | /system
GET    /api/v1/sources                 POST /api/v1/sources
PATCH  /api/v1/sources/{id}            DELETE /api/v1/sources/{id}
POST   /api/v1/sources/{id}/test       (DESCRIBE / HEAD)
GET    /api/v1/cameras?page=&q=&state=&group=
POST   /api/v1/cameras/bulk            {sourceId,count,prefix,...}
POST   /api/v1/cameras/bulk-action     {ids,action: enable|disable|delete|rotate-password}
POST   /api/v1/cameras/{id}/actions/{enable|disable|restart|rotate-password}
GET    /api/v1/cameras/{id}/preview    → rtsp/hls/whep URL kèm credential
GET    /api/v1/cameras/export          CSV
POST   /api/v1/loadtest                {count,concurrency,timeoutMs}
GET    /api/v1/loadtest/{id}
GET    /api/v1/monitor/stream          SSE
POST   /api/v1/mediamtx/auth           (MediaMTX gọi, không cần token)
GET    /healthz | /api/v1/metrics      (Prometheus text)
```

Ngoài `/auth/login`, `/mediamtx/auth`, `/healthz`, mọi endpoint cần `Authorization: Bearer <token>` (hoặc `?token=` cho SSE/CSV).

## Vận hành

```bash
# backup SQLite (VACUUM INTO, an toàn khi đang chạy)
cd deploy && ./scripts/backup.sh          # Windows: .\scripts\backup.ps1

# smoke test end-to-end (login → source → N camera → DESCRIBE load test)
cd deploy && ./scripts/smoke-test.sh      # Windows: .\scripts\smoke-test.ps1 -Cameras 200
```

Biến môi trường chính (xem đầy đủ trong `backend/internal/config/config.go`):

| Biến | Mặc định | Ý nghĩa |
|---|---|---|
| `RC_LISTEN` | `127.0.0.1:8080` | Địa chỉ API/UI |
| `RC_DATA_DIR` | `./data` | Thư mục SQLite + master key |
| `RC_MEDIA_API` | `http://127.0.0.1:9997` | Control API của MediaMTX |
| `RC_MEDIA_RTSP_BASE` | `rtsp://127.0.0.1:8554` | RTSP base cho client nội bộ / load test |
| `RC_MEDIA_RTSP_LOOPBACK` | = `RC_MEDIA_RTSP_BASE` | URL MediaMTX dùng để pull chính nó (bridge dev: `rtsp://127.0.0.1:8554`) |
| `RC_PUBLIC_RTSP` | `localhost:8554` | Host:port trả về cho VMS |
| `RC_PUBLIC_HLS` / `RC_PUBLIC_WEBRTC` | `http://localhost:8888` / `:8889` | URL trả về cho browser |
| `RC_ADMIN_USER` / `RC_ADMIN_PASSWORD` | `admin` / `admin` | Tài khoản UI (đổi trước khi mở mạng) |
| `RC_MASTER_KEY` | tự sinh | 32 byte base64, dùng để giải mã secret |
| `RC_RECONCILE_INTERVAL` | `5s` | Chu kỳ đồng bộ path |
| `RC_MAX_BULK_CAMERAS` | `500` | Giới hạn mỗi lần tạo hàng loạt |
| `RC_IMAGE` | `restreamcamera-api` (build) / `unray/restreamcamera-api` (deploy) | Tên image, build ra hoặc pull về |
| `RC_TAG` | `latest` | Tag image Docker (push chồng mỗi lần release, không theo version) |
| `RC_VERSION` | `dev` | Version nhúng trong binary (`restreamd version`), không ảnh hưởng tag image |

## Kiểm thử

```bash
# trong Docker, không cần Go trên host
docker run --rm -v "$PWD/backend:/src" -w /src golang:1.25-alpine sh -c "go vet ./... && go test ./..."
```

Test hiện có: crypto round-trip/tamper, auth callback (credential, allowlist, origin isolation, redirect), token, slug, reconciler với MediaMTX giả (add/patch/delete/idempotent/publisher/redirect).

## Giới hạn đã biết

- SQLite + single writer: phù hợp ~200 camera / 1 control plane. Trên 1000 camera nên chuyển Postgres (schema đã tách lớp store).
- Không transcode: mọi camera ảo phát đúng codec/độ phân giải của source. Client không hỗ trợ codec đó sẽ không xem được.
- `redirect` mode cho client đọc thẳng origin path (tiết kiệm băng thông control plane) và vì vậy origin path mở read cho các client được redirect.
- WebRTC cần UDP 8189 (hoặc `webrtcAdditionalHosts` đúng địa chỉ) khi chạy sau NAT.
