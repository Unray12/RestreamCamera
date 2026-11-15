# RestreamCamera — Kế hoạch kỹ thuật (Plan v1.1)

> Mục tiêu: nhận **1 hoặc vài luồng camera thật** (RTSP/HTTP/HLS/SRT/file...), sau đó **nhân bản thành hàng trăm luồng "camera ảo"** với URL/credential/metadata riêng, phát lại bằng RTSP/HTTP(HLS/FLV)/WebRTC/SRT để giả lập hàng trăm camera cho mục đích test tải VMS/NVR/AI pipeline.
>
> Trạng thái: **đã hiện thực M0–M4** (backend Go + SQLite + UI nhúng, Docker Compose, test). Kế hoạch này vẫn là tài liệu kiến trúc; xem `README.md` để chạy. Cập nhật: 2026-11-15.
>
> Đã chốt: **Backend Go**, **deploy bằng Docker** (Linux container). Chi tiết Docker ở [§10](#10-docker-deployment-chi-tiết).

---

## 1. Tóm tắt quyết định (TL;DR)

| Hạng mục | Quyết định | Lý do |
|---|---|---|
| Media engine | **MediaMTX** (pin `v1.21.x`) | 1 binary, zero-dep, MIT, RTSP/RTMP/HLS/WebRTC/SRT, Control API v3, Prometheus metrics, on-demand source |
| Ngôn ngữ BE | **Go 1.25** | Cùng hệ sinh thái MediaMTX (gortsplib), 1 binary static, goroutine cho hàng trăm path/health-check |
| UI | **React 19 + TS + Vite + Tailwind + shadcn/ui** | Dev nhanh, player WHEP/HLS sẵn; build tĩnh nhúng vào binary Go |
| DB | **SQLite (modernc, pure-Go)** cho MVP → **PostgreSQL** khi multi-node | Zero-ops, không CGO; compose profile sẵn cho PG |
| Deploy | **Docker Compose, `network_mode: host`** | RTSP-over-UDP & WebRTC ICE không thể NAT qua bridge một cách đáng tin cậy |
| Kiến trúc nhân bản | **Origin 1 lần ingest → N path ảo `sourceOnDemand` trỏ về origin (loopback RTSP)** | Không transcode; MediaMTX tự fan-out packet; chi phí ≈ băng thông |
| Transcode | **Mặc định KHÔNG**. Chỉ re-encode 1 lần ở origin nếu cần OSD/watermark cho cả nhóm | Giữ chi phí O(N) băng thông thay vì O(N) CPU |
| Auth | **MediaMTX auth HTTP callback → BE** (per-camera credential, revoke, audit) + JWT cho UI | 1 nguồn sự thật, không lưu plaintext trong `mediamtx.yml` |
| Scale-out | **Origin + read replicas** (pattern chính thức) khi > ~200–300 luồng egress | Tách băng thông egress theo NIC/máy |
| Mô phỏng nâng cao | ONVIF + WS-Discovery mock (Go) để VMS tự discover camera ảo | Sát thực tế khi test VMS |

---

## 2. Bài toán & phạm vi

### 2.1 Yêu cầu chức năng (in-scope)

1. **Nguồn (Source)**: thêm/sửa/xoá nguồn camera: RTSP pull, RTMP pull, HTTP/HLS, SRT, UDP MPEG-TS, file MP4 loop (ffmpeg), webcam/screen (dev-only).
2. **Camera ảo (Virtual Camera)**: sinh hàng loạt, mỗi camera có:
   - `id`, `name`, `path` (vd `vcam-0001`), credential riêng, group/tag, metadata giả lập (vendor/model/serial/firmware/độ phân giải),
   - chọn nguồn (1 source hoặc 1 clip trong pool), chọn bật/tắt từng giao thức egress,
   - `maxReaders`, IP allowlist, on-demand hay always-on.
3. **Đa giao thức egress** trên cùng 1 path: RTSP/RTSPS, HLS (fMP4/LL-HLS), WebRTC (WHEP), SRT, RTMP, MPEG-TS — MediaMTX làm sẵn.
4. **UI**: dashboard, CRUD source, wizard sinh N camera, bảng camera ảo (filter/paginate), preview (WebRTC/HLS/snapshot), monitor realtime, log/event, import-export CSV/JSON, bulk action (start/stop/restart/delete/rotate credential).
5. **Giám sát**: trạng thái path (ready/not ready), số reader, bitrate in/out, packet loss/jitter, lỗi; CPU/RAM/NIC; ngưỡng cảnh báo; biểu đồ theo thời gian.
6. **Giả lập sự cố**: offline theo lịch, drop ngẫu nhiên, giảm bitrate, freeze frame, mất gói, chậm khởi động.
7. **Load generator**: mở N client RTSP/HLS/WebRTC song song, đo tỉ lệ kết nối thành công, time-to-first-frame, throughput, drop; xuất báo cáo.
8. **ONVIF mock** (phase sau): WS-Discovery + Device/Media service trả `GetStreamUri` trỏ về camera ảo.
9. **API-first**: OpenAPI 3.1, có CLI (`rcctl`) dùng chính API đó.
10. **Export URL**: bảng URL + credential xuất CSV/JSON/M3U để đưa cho đội test.

### 2.2 Ngoài phạm vi (out-of-scope v1)

- Farm transcode/GPU, AI analytics, nhận diện.
- Recording/playback quy mô lớn (chỉ bật cho vài camera nếu cần).
- Multi-tenant SaaS, billing.
- Kubernetes/Helm (chỉ Compose ở v1; K8s để sau — host networking là rào cản chính).
- Thay thế VMS thật.

### 2.3 Tiêu chí nghiệm thu (acceptance)

- `git clone` → `cp .env.example .env` → `docker compose up -d` → UI chạy trong **< 60 s** trên máy sạch.
- 1 camera thật 1080p@4Mbps → sinh **200 camera ảo**, đọc đồng thời bằng `ffplay`/VLC, không mất gói, latency tăng < 300 ms.
- Tạo 200 camera ảo từ UI **< 10 s**; xoá toàn bộ **< 10 s**.
- `docker compose restart mediamtx` → mọi path được reconcile lại, client đọc lại được **< 30 s**, không mất dữ liệu cấu hình.
- Không rò rỉ goroutine/process sau 24h soak; RSS tăng < 5%.
- Kill camera nguồn → camera ảo `not ready`, tự phục hồi khi nguồn trở lại (không cần restart container).
- UI cập nhật trạng thái < 3 s sau thay đổi.

---

## 3. Kiến trúc

### 3.1 Sơ đồ tổng thể (single node, Docker)

```
                    ┌──────────────── UI (React SPA, static) ────────────────┐
                    │ Sources │ Virtual Cameras │ Preview │ Monitor │ Logs    │
                    └───────────────┬───────────────────────────────────────┘
                                    │ HTTPS (Caddy) · REST (JWT) · SSE
                    ┌───────────────▼───────────────────────────────────────┐
                    │  container: api  (Go, host-network, 127.0.0.1:8080)   │
                    │  auth/RBAC · provisioning/reconciler · metrics        │
                    │  scheduler · failure-injection · audit · loadgen      │
                    └───────┬───────────────────────────────┬───────────────┘
                            │ Control API v3 (9997)         │ auth callback
                            │ + /metrics (9998)             │
                    ┌───────▼───────────────────────────────▼───────────────┐
                    │  container: mediamtx  (host-network)                  │
                    │  ingest : src-cam-01 (pull RTSP / publisher / file)   │
                    │  fanout : vcam-0001 … vcam-0200 (sourceOnDemand)      │
                    │  egress : RTSP 8554 · HLS 8888 · WHEP 8889 · SRT 8890 │
                    └───────┬───────────────────────────────────────────────┘
                            │ (host ports, no NAT)
    ┌───────────────────────┼───────────────────────────┐
 Camera thật / clip MP4   VMS/NVR/AI client          Browser (WHEP/HLS)
```

### 3.2 Data plane

```
Camera RTSP ──1 session──► MediaMTX path "src-cam-01" (packet-level, no transcode)
                                   │
                                   ├── vcam-0001  (source=rtsp://127.0.0.1:8554/src-cam-01, on-demand)
                                   ├── vcam-0002
                                   └── … vcam-0200
                                            │
                    ┌───────────────────────┼────────────────────────┐
              RTSP client #1          RTSP client #N            HLS/WHEP viewer
```

Nguyên tắc vàng: **decode/encode 0 lần**. Mọi thứ là remux + copy packet. Chi phí duy nhất là băng thông × số reader + overhead RTSP/RTCP.

### 3.3 Control plane

```
UI ──REST──► API ──(1) validate + ghi DB (desired state)
                  ──(2) reconcile: POST /v3/config/paths/add/{name}  (actual state)
                  ──(3) đọc /v3/paths/list + /metrics để đồng bộ status
                  ──(4) SSE đẩy delta trạng thái về UI
```

**Reconciler**: DB là source of truth, MediaMTX chỉ là runtime. Mỗi 5–10 s (và khi khởi động) so sánh desired vs actual → add/patch/delete path. Nhờ vậy restart container, sửa tay hay mất kết nối đều tự phục hồi. Đây cũng là lý do **không cần volume cho MediaMTX** (xem §10.6).

### 3.4 Vì sao dùng MediaMTX (và không dùng gì khác)

| Giải pháp | RTSP out | HLS | WebRTC | SRT | API điều khiển | Nhận xét |
|---|---|---|---|---|---|---|
| **MediaMTX** | ✅ | ✅ | ✅ | ✅ | ✅ Control API v3 | **Chọn** |
| ZLMediaKit | ✅ | ✅ | ✅ | ✅ | ✅ REST + webhook | Mạnh (GB28181/HTTP-FLV) nhưng C++/deploy nặng, docs tiếng Trung → phương án B |
| SRS | ❌ (ingest only) | ✅ | ✅ | ✅ | ✅ | Không phát RTSP → loại |
| LiveKit / mediasoup / Janus | ❌ | ❌ | ✅ | ❌ | ✅ | Chỉ WebRTC SFU |
| gst-rtsp-server | ✅ | ❌ | ❌ | ❌ | ⚠️ | 1 pipeline/client, không scale |
| nginx-rtmp | ❌ | ✅ | ❌ | ❌ | ❌ | Legacy |
| datarhei Restreamer | ⚠️ | ✅ | ⚠️ | ❌ | ⚠️ | Hướng single-stream |
| Ant Media | ✅ | ✅ | ✅ | ✅ | ✅ | Thương mại, nặng |

### 3.5 Hai chế độ nhân bản

| Mode | Cách làm | Ưu | Nhược |
|---|---|---|---|
| `proxy` (mặc định) | Path ảo `source: rtsp://127.0.0.1:8554/src-xxx`, `sourceOnDemand: yes` | URL/credential/stats riêng, client không cần follow redirect | Tốn thêm 1 lần băng thông loopback (N × bitrate) |
| `redirect` | Path ảo `source: redirect` → origin path | Không nhân đôi băng thông | Nhiều VMS không follow RTSP redirect |

### 3.6 Tối ưu tương lai (khi loopback thành nút cổ chai)

1. **Read replicas**: origin + N replica (`source: rtsp://origin:8554/$G1, sourceOnDemand: yes`), LB L4 (RTSP/SRT/RTMP) và L7 sticky (HLS/WHEP).
2. **Fan-out core bằng gortsplib**: 1 upstream session → N downstream session trong cùng process. ~3–4 tuần, chỉ làm khi benchmark chứng minh cần.
3. **Fork MediaMTX** thêm `aliasOf` cho path (dùng chung `*stream.Stream`).

---

## 4. Tech stack

### 4.1 Backend (Go)

| Thành phần | Chọn | Ghi chú |
|---|---|---|
| HTTP router | `chi` | Nhẹ, stdlib-compatible |
| API contract | OpenAPI 3.1 + `oapi-codegen` | Sinh type + server interface, FE sinh client |
| DB access | `sqlc` + `database/sql` | Compile-time SQL |
| Migration | `goose` (embed) | Chạy tự động khi start |
| SQLite | `modernc.org/sqlite` | Pure Go, `CGO_ENABLED=0`, image nhỏ |
| Logging | `log/slog` (JSON) | `trace_id`, `camera_id` |
| Metrics | `prometheus/client_golang` + scrape MediaMTX `/metrics` | 1 endpoint `/metrics` |
| Live update | SSE | Đơn giản hơn WS, đủ cho status/telemetry |
| CLI | `cobra` | `rcctl cams create --count 200` |
| RTSP client (loadgen) | `gortsplib/v5` | Cùng lib MediaMTX dùng |

### 4.2 Frontend

React 19 + TypeScript + Vite, TanStack Query/Table, React Router, RHF + Zod, Tailwind + shadcn/ui, Recharts.
Preview: nhúng player WebRTC của MediaMTX (`http://host:8889/<path>?controls=false`) và HLS (`http://host:8888/<path>/index.m3u8`). Grid giới hạn 9–16 WHEP cùng lúc, còn lại dùng snapshot.

Build ra `dist/` → **nhúng vào binary Go** bằng `embed.FS` → image API chỉ có 1 process, 1 binary.

### 4.3 Media node

MediaMTX pin `v1.21.x` (khoá trong `deploy/versions.env`): `api: yes` (localhost), `metrics: yes`, `authMethod: http`, `rtspTransports: [tcp, udp]`, `hlsVariant: fmp4`, `webrtcAdditionalHosts`, `pprof` chỉ bật ở dev.

### 4.4 Hạ tầng

**Docker-first**: mọi thứ chạy bằng Compose trên Linux (xem §10). Không cần cài Go/Node/MediaMTX trên host — chỉ cần Docker Engine + Compose v2.
Windows dev: WSL2 + Docker Engine trong distro (ổn cho dev); production nên là Linux host (host networking + UDP ổn định).

---

## 5. Mô hình dữ liệu

```
sources(id, name, type[rtsp|rtmp|http|hls|srt|mpegts|file|webcam], url, username,
        password_enc, transport[tcp|udp], probe_json, status, last_seen_at,
        created_at, updated_at)

clips(id, name, file_path, duration_s, codec, width, height, fps, bitrate_kbps)

groups(id, name, description, color)

virtual_cameras(id, source_id?, clip_id?, group_id?, name, path_slug, enabled,
        mode[proxy|redirect], source_on_demand, always_on, max_readers,
        rtsp_enabled, hls_enabled, webrtc_enabled, srt_enabled, rtmp_enabled,
        read_user, read_pass_enc, ip_allowlist, vendor, model, serial, firmware,
        profile_json, created_at, updated_at)   -- unique(path_slug)

camera_runtime(camera_id PK, state[offline|starting|ready|error], readers,
        inbound_kbps, outbound_kbps, packets_lost, last_error, updated_at)

media_nodes(id, name, base_url, api_url, metrics_url, public_host, capacity,
        version, healthy, last_heartbeat)

events(id, ts, level, category, camera_id?, source_id?, actor, message, detail_json)

failure_scenarios(id, name, target_selector_json, kind[offline|drop|freeze|bitrate|
        latency|packet_loss], params_json, cron/interval, enabled)

users(id, email, password_hash[argon2id], role[admin|operator|viewer], created_at)
audit_log(id, ts, user_id, action, target, before_json, after_json, ip)

settings(key, value_json)
```

Quy ước: **mọi secret mã hoá at-rest** (AES-256-GCM, key từ Docker secret/`RC_MASTER_KEY`); UI chỉ trả về mask. DB volume nằm trên **local disk** (không dùng NFS/SMB cho SQLite).

---

## 6. API (REST, /api/v1)

| Method | Path | Mô tả |
|---|---|---|
| POST | `/auth/login` / `/auth/refresh` | JWT (access 15m, refresh 7d) |
| GET/POST | `/sources` | list/create; `POST /sources/{id}/probe` chạy ffprobe |
| GET/PATCH/DELETE | `/sources/{id}` | CRUD |
| POST | `/sources/{id}/test` | thử kết nối + lấy SDP/codec |
| GET/POST | `/cameras` | list (filter `group`, `state`, `q`, paging) / create 1 |
| POST | `/cameras/bulk` | `{count, namePattern, sourceId|clipId, groupId, startIndex, protocols[]}` → tạo N |
| GET/PATCH/DELETE | `/cameras/{id}` | CRUD |
| POST | `/cameras/{id}/actions/{start|stop|restart|rotate-credential}` | |
| POST | `/cameras/bulk-action` | `{ids[]|filter, action}` |
| GET | `/cameras/{id}/preview` | `{whepUrl, hlsUrl, snapshotUrl}` |
| GET | `/cameras/{id}/metrics?from&to` | timeseries |
| GET | `/cameras/export?format=csv|json|m3u` | URL + credential |
| GET/POST | `/groups`, `/clips` | |
| GET | `/events` | filter + paging |
| GET | `/monitor/overview` | tổng hợp node + readers/bitrate |
| GET | `/monitor/stream` (SSE) | delta runtime 1–2 s |
| GET/POST | `/scenarios` | kịch bản sự cố |
| POST/GET | `/loadtest/runs`, `/loadtest/runs/{id}` | chạy/đọc kết quả |
| GET | `/settings`, `PATCH /settings` | |
| GET | `/healthz`, `/metrics`, `/api/v1/mediamtx/auth` | probe, Prometheus, auth callback |

Chuẩn lỗi: `application/problem+json` (RFC 9457). Paging cursor-based. Idempotency-Key cho bulk create.

---

## 7. UI — màn hình

1. **Login**
2. **Dashboard**: camera ready/offline, tổng reader, bitrate egress, CPU/RAM node, cảnh báo, sparkline 24h.
3. **Sources**: bảng + trạng thái probe, nút "Test", snapshot.
4. **Virtual Cameras**: bảng virtualized — name/path/source/group/state/readers/bitrate/protocols; filter, bulk action, copy URL.
5. **Create wizard**: source/clip → pattern tên (`{prefix}-{i:04d}`) → số lượng (1–500) → protocol → credential policy → preview → tạo (progress bar).
6. **Camera detail**: preview WHEP, SDP/codec, URL từng protocol (copy/QR), metrics chart, event log, nút giả lập lỗi.
7. **Monitor**: chart đa camera, top-N bitrate, bảng reader, chọn khoảng thời gian.
8. **Scenarios**: lịch offline/drop/freeze.
9. **Load test**: N client × duration × protocol, tiến trình realtime, báo cáo cuối.
10. **Settings/System**: media node, auth, retention events, export/import, version info.

UX: URL state cho mọi bảng (share link), dark mode, i18n VI/EN.

---

## 8. Luồng xử lý

### 8.1 Tạo 200 camera ảo
```
UI ──POST /cameras/bulk──► API
  validate count ≤ limit, pattern hợp lệ, path_slug chưa tồn tại
  insert 200 row (1 transaction, batch)
  enqueue 200 job reconcile (worker pool 8–16, rate-limit ~50 path/s)
  mỗi job: POST /v3/config/paths/add/{slug}
  camera_runtime = starting
  SSE đẩy {created: 137/200}
MediaMTX nhận path (on-demand) → chưa tốn tài nguyên tới khi có reader
```

### 8.2 Client đọc camera ảo (on-demand)
```
VLC ──DESCRIBE rtsp://host:8554/vcam-0042──► MediaMTX
  MediaMTX ──auth HTTP callback──► API  (user/pass, path, action=read)
      API: hash + ip_allowlist + enabled → 200/401
  path on-demand → mở source rtsp://127.0.0.1:8554/src-cam-01
  nhận SDP → trả SDP → PLAY → copy packet 1→1
  client ngắt & hết reader sau 15s → đóng source (giải phóng)
```

### 8.3 Mất nguồn / phục hồi
```
Camera offline → path src-cam-01 not ready
API poll 2s → camera ảo dùng source đó = error, last_error="source offline"
SSE → UI đỏ + toast
Camera online → MediaMTX tự reconnect → path ảo phục vụ lại (on-demand khi có reader)
```

### 8.4 Giả lập sự cố
- `offline`: disable path ảo → client bị từ chối/404.
- `freeze`: dừng publisher nhưng giữ path "ready" một lúc → test timeout detection.
- `packet_loss/jitter/latency`: proxy UDP nhỏ (Go) hoặc `tc netem` (Linux host).
- `bitrate drop`: chuyển sang clip bitrate thấp (pool clip chuẩn bị sẵn) — không transcode runtime.
- `schedule`: cron per scenario.

### 8.5 Multi-node (M5)
```
Control plane host: api + postgres + caddy
   ├── media-node-01: mediamtx + agent   (0–150 cam)
   ├── media-node-02: mediamtx + agent
   └── media-node-03: mediamtx + agent
```

---

## 9. Năng lực & sizing

Giả định H.264, không transcode, client đọc đồng thời.

| Profile | Bitrate/luồng | 200 reader egress | + loopback (proxy) | NIC |
|---|---|---|---|---|
| 1080p | 4 Mbps | 800 Mbps | ~1.6 Gbps | 10 GbE |
| 720p | 1.5 Mbps | 300 Mbps | ~600 Mbps | 1 GbE (khít) |
| 720p low | 1 Mbps | 200 Mbps | ~400 Mbps | 1 GbE |
| 100 reader @1080p | 4 Mbps | 400 Mbps | ~800 Mbps | 1 GbE |

Gợi ý VM/host cho Docker:
- **200 × 1080p**: 8 vCPU, 8 GB RAM, 10 GbE, SSD 50 GB.
- **200 × 720p**: 4 vCPU, 4 GB RAM, 1 GbE (khuyến nghị 2.5 GbE cho dư địa).
- **500+ luồng**: 2+ media node (multi-node ở §10.10).

Ước lượng (phải đo ở M0): CPU ~1 vCPU cho 1–2 Gbps copy; RAM ~0.5–1 MB/path + buffer/session (200 path ≈ 300–800 MB); mỗi RTSP UDP session dùng 2 ephemeral UDP port/server.

Nút cổ chai: **băng thông + port/NIC**, không phải CPU → mặc định 720p 1–1.5 Mbps cho demo 200 camera.

### Tuning host (không phải trong container)
- Linux: `ulimit -n 65535`, `fs.file-max`, `net.core.somaxconn`, `net.ipv4.ip_local_port_range`, `nf_conntrack_max`, `net.core.rmem_max/wmem_max`, `ethtool -L`/RSS.
- Windows/WSL2: mở dynamic port range, firewall cho 8554/8322/8888/8889/8890 + UDP ephemeral, exclude Defender cho volume dữ liệu.
- NTP/chrony đồng bộ giờ (timestamp RTSP).

---

## 10. Docker deployment (chi tiết)

### 10.1 Nguyên tắc

1. **Một image cho API + UI** (Go embed) → image ~30 MB, `CGO_ENABLED=0`.
2. **MediaMTX dùng image chính thức**, pin version, không build lại, **không volume config ghi được**.
3. **`network_mode: host`** cho `mediamtx` và `api` — bắt buộc để RTSP-over-UDP, WebRTC ICE, SRT hoạt động không cần NAT.
4. **Không persistence ở MediaMTX**: path ảo do reconciler tạo lại từ DB sau restart.
5. **Mọi secret qua Docker secret/file**, không nằm trong image hay compose git.

### 10.2 Topology mạng — quyết định quan trọng nhất

| Phương án | RTSP/UDP | WebRTC | SRT | Ghi chú |
|---|---|---|---|---|
| **A. `network_mode: host` (chọn)** | ✅ | ✅ | ✅ | Không NAT; API/metrics bind `127.0.0.1` → không lộ ra LAN. Mỗi host 1 media node |
| B. Bridge + publish port | ❌ (chỉ TCP interleaved) | ⚠️ cần `webrtcAdditionalHosts` + TURN | ✅ (1 UDP port) | Dùng cho dev hoặc host dùng chung |
| C. Hybrid (mediamtx host, api bridge) | ✅ | ✅ | ✅ | API phải bind docker0 gateway → bớt an toàn; chỉ khi bắt buộc tách network |

**Chọn A.** Hệ quả cần biết:
- Không dùng `ports:` được nữa → firewall trên host là “cổng vào” duy nhất.
- Không `--scale` được service host-network → mỗi host 1 mediamtx (multi-node = nhiều host, §10.10).
- MediaMTX cấp **UDP ephemeral port** cho mỗi RTP/RTCP session (không cấu hình range được ở v1.21) → firewall phải mở dải ephemeral cho subnet client, **hoặc** ép client dùng RTSP over TCP (`rtspTransports: [tcp]`) khi mạng khắt khe.
- Nếu host chia sẻ với dịch vụ khác: kiểm tra trùng cổng 8554/8888/8889/8890/8080/443.

### 10.3 Cổng sử dụng

| Cổng | Proto | Dịch vụ | Expose |
|---|---|---|---|
| 80/443 | TCP | Caddy → API/UI | LAN/WAN (tuỳ chọn TLS) |
| 8080 | TCP | API (Go) | **127.0.0.1** (sau Caddy) |
| 8554 | TCP/UDP | RTSP | LAN (client test) |
| 8322 | TCP | RTSPS (tuỳ chọn) | LAN |
| 8888 | TCP | HLS | LAN |
| 8889 | TCP | WebRTC/WHEP (HTTP signaling) | LAN |
| 8189 | UDP | WebRTC ICE | LAN |
| 8890 | UDP | SRT | LAN |
| 1935 | TCP | RTMP (nếu bật) | LAN |
| 9997 | TCP | Control API | **127.0.0.1** |
| 9998 | TCP | Metrics | **127.0.0.1** |
| ephemeral | UDP | RTP/RTCP server-side | LAN (dải ephemeral) |

### 10.4 Cấu trúc file deploy

```
deploy/
├─ docker-compose.yml            # production: host network
├─ docker-compose.dev.yml        # dev: bridge, RTSP TCP-only, hot reload
├─ docker-compose.multi-node.yml # control-plane + agent
├─ docker-compose.observability.yml  # profile prometheus/grafana
├─ mediamtx.template.yml         # config tĩnh (origin path khai báo tay)
├─ Caddyfile
├─ .env.example
├─ versions.env                  # RC_VERSION=1.0.0, MTX_VERSION=1.21.3
├─ windows/install.ps1           # Docker Desktop/WSL2 bootstrap (dev)
└─ scripts/
   ├─ backup.sh                  # sqlite .backup + export json
   ├─ tune-host.sh              # sysctl/ulimit/firewall
   └─ smoke-test.sh             # ffprobe 200 URL, report
backend/Dockerfile
backend/Dockerfile.agent?        # cùng image, khác command
web/ (build trong stage của backend/Dockerfile)
```

### 10.5 Dockerfile (multi-stage, 1 image cho API+UI)

```dockerfile
# ---------- web ----------
FROM node:22-alpine AS web
WORKDIR /src
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build            # -> /src/dist

# ---------- build ----------
FROM golang:1.25-alpine AS build
ARG VERSION=dev
RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
COPY --from=web /src/dist ./internal/webui/dist
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/restreamd ./cmd/restreamd

# ---------- runtime ----------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/restreamd /restreamd
USER nonroot:nonroot
ENTRYPOINT ["/restreamd"]
HEALTHCHECK --interval=15s --timeout=3s --start-period=20s --retries=3 \
  CMD ["/restreamd", "healthcheck"]
```

Điểm cần lưu ý:
- `distroless` không có shell → healthcheck phải là **subcommand của binary** (`restreamd healthcheck` gọi `GET 127.0.0.1:8080/healthz` + `GET 9997/v3/info`).
- Log JSON ra stdout/stderr → Docker logging driver.
- Không chạy root; `read_only` rootfs được (xem §10.8).
- Image agent dùng **cùng image**, override `command: ["/restreamd","agent","--control","https://cp:8080"]`.

### 10.6 Compose production (rút gọn, định hướng)

```yaml
name: restreamcamera

services:
  mediamtx:
    image: bluenviron/mediamtx:${MTX_VERSION}
    network_mode: host
    restart: unless-stopped
    read_only: true
    tmpfs: ["/tmp"]
    user: "1000:1000"
    volumes:
      - ./mediamtx.yml:/mediamtx.yml:ro
      - ./data/recordings:/recordings      # chỉ khi bật record
    ulimits:
      nofile: 65536
    security_opt: ["no-new-privileges:true"]
    cap_drop: ["ALL"]
    logging:
      driver: json-file
      options: { max-size: "50m", max-file: "5" }

  api:
    image: ghcr.io/aclab/restreamcamera:${RC_VERSION}
    network_mode: host
    restart: unless-stopped
    depends_on: [mediamtx]
    environment:
      RC_LISTEN: 127.0.0.1:8080
      RC_MEDIA_API: http://127.0.0.1:9997
      RC_MEDIA_METRICS: http://127.0.0.1:9998/metrics
      RC_DB_DRIVER: sqlite
      RC_DB_DSN: /data/restream.db
      RC_MEDIA_AUTH_MODE: callback     # token chia sẻ với mediamtx
    volumes:
      - ./data/app:/data               # SQLite + export + snapshot cache
      - ./data/clips:/clips:ro         # clip MP4 để loop
    secrets: [rc_master_key, rc_media_auth_token]
    healthcheck:
      test: ["CMD", "/restreamd", "healthcheck"]
      interval: 15s
      timeout: 5s
      retries: 3
    ulimits:
      nofile: 65536

  caddy:
    image: caddy:2-alpine
    network_mode: host            # đơn giản nhất khi api bind 127.0.0.1
    restart: unless-stopped
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - ./data/caddy:/data
    depends_on: [api]

secrets:
  rc_master_key:
    file: ./secrets/master_key
  rc_media_auth_token:
    file: ./secrets/media_auth_token
```

Ghi chú:
- **Không có `ports:`** vì host network; thay bằng firewall host (§10.7).
- `depends_on` chỉ là thứ tự start; API **phải** chịu được MediaMTX chưa sẵn sàng (retry + reconciler) — đừng dựa vào healthcheck của service kia.
- `./mediamtx.yml` mount `:ro`: Control API add path chỉ đổi in-memory; nếu bản nâng cấp sau này ghi file, reconciler vẫn đúng nhưng mount `:ro` sẽ báo lỗi → test ở M0.
- `user: 1000:1000` cho mediamtx để ghi recordings; nếu không record thì bỏ.
- Với `read_only: true`, MediaMTX cần `tmpfs /tmp`; API cần `/data` writable (SQLite WAL).

### 10.7 Firewall/network trên host

```bash
# ví dụ nftables/ufw — mở cho subnet client test
ufw allow from 10.20.0.0/16 to any port 8554 proto tcp
ufw allow from 10.20.0.0/16 to any port 8554 proto udp
ufw allow from 10.20.0.0/16 to any port 8888 proto tcp
ufw allow from 10.20.0.0/16 to any port 8889 proto tcp
ufw allow from 10.20.0.0/16 to any port 8189 proto udp
ufw allow from 10.20.0.0/16 to any port 8890 proto udp
ufw allow from 10.20.0.0/16 to any port 443  proto tcp
# RTSP RTP/RTCP: dải ephemeral (vd 32768-60999) — hoặc ép TCP interleaved
ufw allow from 10.20.0.0/16 to any port 32768:60999 proto udp
```

Nếu khách hàng không cho mở dải UDP lớn: đặt `rtspTransports: [tcp]` trong `mediamtx.yml` và nêu rõ trong RUNBOOK (mất khả năng UDP, tăng latency nhẹ, CPU cao hơn chút).

### 10.8 Security trong container

- `api`/`mediamtx`: `cap_drop: [ALL]`, `no-new-privileges`, `read_only: true` + tmpfs, non-root.
- API & metrics bind `127.0.0.1` → nằm ngoài tầm với LAN (host network vẫn an toàn vì bind loopback).
- Caddy: TLS tự động (internal CA hoặc ACME), HTTP basic/OIDC ở tầng Caddy nếu muốn lớp bảo vệ thứ 2; app vẫn có JWT + RBAC.
- Không mount `/var/run/docker.sock`; không `privileged`.
- Secret files quyền `0400`, `.gitignore` cho `secrets/`, `data/`, `.env`.
- Rate limit auth callback (tránh brute-force credential camera ảo) — dùng `chi/middleware` + token bucket theo IP.

### 10.9 Vòng đời & vận hành

| Việc | Cách làm |
|---|---|
| Start | `docker compose up -d --wait` (healthcheck API) |
| Log | `docker compose logs -f api mediamtx` (JSON, có `trace_id`) |
| Update | sửa `versions.env` → `docker compose pull && docker compose up -d` |
| Rollback | đổi tag về bản trước → `up -d`; DB migration phải **backward-compatible 1 version** (expand/contract) |
| Restart media | `docker compose restart mediamtx` → reconciler tạo lại path 10–30 s |
| Backup | cron host: `docker compose exec -T api /restreamd backup /data/backup/$(date +%F).db` (dùng `VACUUM INTO`) + copy `data/app` |
| Restore | stop api → thay file → start → reconciler sync MediaMTX |
| Disk | `data/app` (DB + snapshot cache) nhỏ; `data/recordings` tách volume + quota; alert > 80% |
| Upgrade MediaMTX | pin version, chạy integration test với version mới trước khi đổi `MTX_VERSION` |
| Debug | `pprof` chỉ bật compose dev; `docker compose exec api /restreamd debug paths` in desired/actual diff |

### 10.10 Multi-node compose (M5)

```yaml
# control-plane host: api + postgres + caddy
# media-node host:   mediamtx + agent (cùng image api)
services:
  agent:
    image: ghcr.io/aclab/restreamcamera:${RC_VERSION}
    command: ["/restreamd", "agent", "--control", "https://cp.internal:443", "--node", "mn-01"]
    network_mode: host
    environment:
      RC_MEDIA_API: http://127.0.0.1:9997
      RC_MEDIA_METRICS: http://127.0.0.1:9998/metrics
    secrets: [rc_agent_token]
```

- Control plane: Postgres (`--profile postgres`), API, Caddy.
- Agent: nhận desired state (kèm hash), gọi Control API local, đẩy metrics/heartbeat.
- `public_host` per node để URL export đúng (vd `rtsp://mn-01.internal:8554/vcam-0042`).
- Origin/replica: origin ở node A (source thật), replica ở node B/C dùng regex path `~^(.+)$` + `sourceOnDemand: yes` trỏ về origin; LB L4/L7 như docs MediaMTX.

### 10.11 Dev compose

`docker-compose.dev.yml`: bridge network, `rtspTransports: [tcp]`, publish `8080/8554/8888/8889/9997`, volume bind source code, `air` hot reload cho Go, `node` + Vite dev server (proxy `/api`), ffmpeg loop publisher cho clip test. Dùng `docker compose -f docker-compose.dev.yml up`.

### 10.12 CI/CD

- GitHub Actions: `lint (golangci-lint) → test (go test + compose integration) → build (buildx multi-arch amd64/arm64) → scan (Trivy) → push ghcr.io` với tag `{semver}`, `sha-xxxx`, `latest`.
- Cache `go mod` + `npm ci`; build ~2–3 phút.
- SBOM (syft) + provenance attestation (tuỳ chọn).
- MediaMTX image không build, chỉ pin digest trong `versions.env` để reproducible.

### 10.13 Windows/WSL2 (dev)

- Khuyến nghị: **WSL2 Ubuntu + Docker Engine cài trong distro**, repo nằm trong filesystem WSL (`~/src`) để tránh I/O chậm.
- `network_mode: host` hoạt động trong WSL2 (Linux kernel), nhưng **UDP từ LAN vào WSL2 có thể không được forward** → test UDP RTSP bằng client chạy trong WSL2, hoặc dùng Linux host.
- Nếu buộc dùng Docker Desktop: dùng `docker-compose.dev.yml` (bridge, RTSP TCP-only) và ghi rõ giới hạn.
- Production: **Linux host** (Ubuntu 22.04/24.04 hoặc Debian 12), Docker Engine 27+, Compose v2.

---

## 11. Cấu trúc repo

```
RestreamCamera/
├─ docs/
│  ├─ PLAN.md                 ← tài liệu này (gồm cả Docker §10)
│  ├─ ARCHITECTURE.md         ← (M1) chi tiết thành phần, sequence diagram
│  ├─ API.md                  ← (M1) sinh từ OpenAPI
│  ├─ RUNBOOK.md              ← (M5) vận hành, tuning, troubleshooting
│  └─ adr/                    ← 0001-media-engine, 0002-fanout-mode, 0003-docker-network
├─ backend/
│  ├─ Dockerfile              ← multi-stage: web + go + distroless
│  ├─ cmd/restreamd/          ← server + subcommand healthcheck/backup/agent
│  ├─ cmd/rcctl/              ← CLI
│  ├─ internal/
│  │  ├─ api/ domain/ store/ mediamtx/ provision/ monitor/ probe/
│  │  ├─ simulate/ onvif/ loadgen/ auth/ config/ webui/ (embed)
│  │  └─ agent/               ← (M5)
│  ├─ openapi/openapi.yaml
│  └─ migrations/
├─ web/                       ← React app (build trong Dockerfile)
├─ deploy/                    ← compose, mediamtx.yml, Caddyfile, scripts (§10.4)
├─ test/
│  ├─ integration/            ← compose up + ffmpeg + ffprobe assert
│  └─ soak/
└─ README.md
```

---

## 12. Cấu hình MediaMTX mẫu

```yaml
logLevel: info
api: yes
apiAddress: 127.0.0.1:9997
metrics: yes
metricsAddress: 127.0.0.1:9998
pprof: no

rtsp: yes
rtspAddress: :8554
rtspTransports: [tcp, udp]
rtspsAddress: :8322          # bật nếu cần RTSPS
hls: yes
hlsAddress: :8888
hlsVariant: fmp4
webrtc: yes
webrtcAddress: :8889
webrtcAdditionalHosts: [192.168.1.10]
srt: yes
srtAddress: :8890

authMethod: http
authHTTPAddress: http://127.0.0.1:8080/api/v1/mediamtx/auth

pathDefaults:
  sourceOnDemand: yes
  sourceOnDemandStartTimeout: 10s
  sourceOnDemandCloseAfter: 15s
  maxReaders: 0

paths:
  src-cam-01:
    source: rtsp://user:pass@192.168.1.50:554/Streaming/Channels/101
    rtspTransport: tcp
    sourceOnDemand: no
    record: no
```

Path ảo tạo runtime bởi API:
```
POST http://127.0.0.1:9997/v3/config/paths/add/vcam-0001
{ "source": "rtsp://127.0.0.1:8554/src-cam-01", "sourceOnDemand": true,
  "maxReaders": 4, "sourceOnDemandCloseAfter": "15s" }
```
Kiểm tra: `GET /v3/paths/list`, `GET /v3/rtsp/sessions/list`, `GET :9998/metrics?type=paths`.

Lưu ý: route/field có thể đổi giữa minor version → **pin version + integration test**.

---

## 13. Lộ trình

| Mốc | Thời lượng | Nội dung | Nghiệm thu |
|---|---|---|---|
| **M0 — Docker spike & chốt số** | 3–5 ngày | Compose (host network) + MediaMTX + script sinh 50 path; ffmpeg loop clip; đo CPU/RAM/băng thông/latency; kiểm chứng UDP qua Docker, hành vi `:ro` config, ephemeral port; so `proxy` vs `redirect` | Báo cáo benchmark + ADR 0003 chốt topology mạng & profile mặc định |
| **M1 — Core BE + image** | 2–2.5 tuần | Go skeleton, OpenAPI, DB/migration, source CRUD + probe, MediaMTX client, reconciler, auth callback, JWT/RBAC, SSE, Dockerfile multi-stage, compose prod/dev, healthcheck subcommand, CI build | `docker compose up -d` trên máy sạch; tạo 200 path qua API; `restart mediamtx` tự phục hồi |
| **M2 — UI v1** | 2 tuần | Login, dashboard, sources, camera table, bulk wizard, detail + preview WHEP/HLS/snapshot, copy/export URL | Thao tác end-to-end qua UI, không cần curl |
| **M3 — Monitor & simulate** | 1.5 tuần | Scrape metrics, timeseries, chart, alert, event log, failure scenarios, loadgen + báo cáo | 200 camera soak 24h (<5% RAM drift); báo cáo load test |
| **M4 — ONVIF & đa giao thức** | 1.5 tuần | WS-Discovery, Device/Media service, GetStreamUri, metadata giả lập, export QR/label | ONVIF Device Manager discover 200 camera ảo, xem stream |
| **M5 — Scale & hardening** | 2 tuần | Multi-node agent + Postgres, origin/replica, L4/L7 LB, Caddy TLS, backup/restore, observability profile (Prometheus/Grafana), security review, RUNBOOK | 2 node × 200 luồng; backup/restore diễn tập thành công; tài liệu bàn giao |

Tổng: **~10–12 tuần** cho 1 dev Go full-stack; **7–8 tuần** nếu 2 người (1 BE/media + 1 FE).

---

## 14. Kiểm thử

1. **Unit**: domain rules, slug generator, credential policy, reconciler state machine (fake MediaMTX client), config/env parsing.
2. **Integration (Docker)**: `docker compose -f docker-compose.test.yml up --abort-on-container-exit` — MediaMTX thật + ffmpeg clip loop → tạo/xoá 50 path, `ffprobe` từng URL, assert codec, assert cleanup.
3. **Load**: `loadgen` mở N RTSP client (TCP interleaved) trong T giây; đo success %, TTFF, bitrate, drop; HLS: N HTTP client; WHEP: pion client/headless.
4. **Soak 24–72h** trong container: 200 path + 50 reader; theo dõi RSS/goroutine/FD/packet loss.
5. **Chaos**: tắt camera nguồn, `docker compose restart mediamtx`, kill api, đầy disk, mất mạng 30s → tự phục hồi + cảnh báo.
6. **Docker-specific**: container restart loop, `read_only` + tmpfs, secret rotation, upgrade/rollback, image scan, host reboot (compose `restart: unless-stopped` + Docker service enabled).
7. **Security**: secret không lộ log/response; rate limit auth; path traversal slug; SSRF khi nhập URL source (allowlist scheme/host).
8. **Compatibility**: VLC, ffplay, GStreamer, ONVIF Device Manager, Frigate/Milestone (nếu có), Chrome/Edge (WHEP/HLS).

---

## 15. Rủi ro & đối sách

| Rủi ro | Ảnh hưởng | Đối sách |
|---|---|---|
| **Bridge network làm hỏng RTSP-over-UDP** | Client không xem được bằng UDP | `network_mode: host` (đã chọn); fallback `rtspTransports: [tcp]` |
| Dải UDP ephemeral bị firewall chặn | Kết nối UDP fail ngẫu nhiên | Mở dải ephemeral cho subnet test; hoặc ép TCP; ghi rõ trong RUNBOOK |
| Host-network port conflict (host dùng chung) | Không start được | Checklist cổng trước khi deploy; hoặc đổi cổng qua config |
| WSL2/Docker Desktop không forward UDP từ LAN | Test UDP fail trên máy dev Windows | Dev dùng TCP-only; production Linux host |
| Băng thông loopback × N | Không đạt 200 × 1080p | Profile 720p; `redirect`; read replicas; fan-out core |
| MediaMTX đổi API/config giữa version | Vỡ provisioning | Pin version + adapter `internal/mediamtx` + integration test |
| VMS không follow redirect / không hỗ trợ WHEP | Không demo được | Mặc định `proxy`; RTSP/HLS là chính; test VMS thật ở M3 |
| Credential plaintext | Lộ secret | Auth HTTP callback; secret file; mã hoá at-rest |
| 1 process MediaMTX là SPOF | Mất toàn bộ khi crash | `restart: unless-stopped` + reconciler; M5 multi-node |
| Transcode ngoài ý muốn | CPU 100% | Cấm transcode mặc định; OSD chỉ 1 lần/nhóm ở origin; alert CPU > 70% |
| Disk đầy (record/HLS) | Dừng dịch vụ | Không record mặc định; volume riêng + quota + alert |
| SQLite trên NFS/SMB | DB corrupt | Bắt buộc local volume; multi-node → Postgres |
| Migration làm hỏng DB khi rollback | Mất dữ liệu | Expand/contract, backup trước upgrade, test restore |
| ONVIF phức tạp | Trễ M4 | Spike 3 ngày; fallback export URL để import vào VMS |

---

## 16. Kỹ năng / nguồn lực

- **Go**: HTTP/API, concurrency (worker pool, context, rate limit), SQL, embed.
- **Media protocol**: RTSP/RTP/RTCP, SDP, H.264 NAL/GOP, HLS, WebRTC/WHEP (đủ debug, không tự viết stack).
- **Docker**: multi-stage build, network modes, secrets, healthcheck, compose profiles, logging driver, Trivy/CI.
- **Frontend**: React + data fetching + video player.
- **Hạ tầng/perf**: Linux network tuning, firewall, Prometheus/Grafana, load test, pprof.

---

## 17. Câu hỏi cần chốt (còn lại)

1. Quy mô mục tiêu: **200 / 500 / 1000 luồng**, profile bitrate-resolution?
2. Client đích là VMS nào — cần **ONVIF** không? Có cần RTSP-over-UDP hay TCP là đủ?
3. Có cần **nhiều clip/cảnh khác nhau** cho các nhóm camera, hay 1 nguồn là đủ?
4. Host production có cho phép **`network_mode: host`** và mở dải UDP ephemeral không? (nếu không → TCP-only)
5. Cần **TLS/domain nội bộ** cho UI/API không (Caddy internal CA hay ACME)?
6. DB: **SQLite** (mặc định) hay **Postgres ngay từ M1** (chuẩn bị multi-node)?
7. Có cần recording/playback cho vài camera không?

---

## 18. Bước tiếp theo

1. Tạo `docs/adr/0001-media-engine.md`, `0002-fanout-mode.md`, `0003-docker-network.md`.
2. M0 spike: `deploy/docker-compose.spike.yml` (host network) + script bash sinh 50 path + `ffprobe` loop + đo `pidstat/docker stats`/`iftop`.
3. Chốt số liệu → cập nhật §9 và chọn profile mặc định.
4. Khởi tạo `backend/` (Go module + layout + Dockerfile) và `web/` (Vite) → bắt đầu M1.
