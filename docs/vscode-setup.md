# VS Code setup — `chatLanguageModels.json`

Hướng dẫn kết nối VS Code Copilot Chat tới **devin-to-api** qua file cấu
hình custom language models.

> **Session mode:** với backend `acp` (mặc định trong docker-compose), mỗi
> *chat mới* trong VS Code được gắn một **devin CLI session riêng** — Devin
> giữ context server-side giữa các turn, không cần client gửi lại toàn bộ
> history. Chi tiết: [README — Backends](../README.md#backends-connect-vs-acp-session-mode).

## 1. Vị trí file

| OS | Path |
| --- | --- |
| Windows | `%APPDATA%\Code\User\chatLanguageModels.json` → `C:\Users\<user>\AppData\Roaming\Code\User\chatLanguageModels.json` |
| macOS | `~/Library/Application Support/Code/User/chatLanguageModels.json` |
| Linux | `~/.config/Code/User/chatLanguageModels.json` |

(VS Code Insiders: đổi `Code` → `Code - Insiders`.)

Cách mở nhanh — trong VS Code: `Ctrl+Shift+P` →
**Chat: Manage Language Models** → bấm icon edit ở provider entry,
hoặc **Preferences: Open User Settings (JSON)** nếu file chưa tồn tại thì
mở trực tiếp theo path trên và tạo mới.

## 2. Cấu trúc file — QUAN TRỌNG

Root của file là một **JSON array** `[ ... ]` chứa các provider object —
không phải một object đơn. Nếu đã có provider khác, thêm Devin như một
phần tử mới của array:

```json
[
    {
        "name": "OtherProvider",
        "...": "..."
    },
    {
        "name": "Devin",
        "vendor": "customendpoint",
        "apiKey": "sk-devin-...",
        "apiType": "chat-completions",
        "models": [ ... ]
    }
]
```

## 3. Provider entry cho devin-to-api

```json
{
    "name": "Devin",
    "vendor": "customendpoint",
    "apiKey": "<PROXY_API_KEY>",
    "apiType": "chat-completions",
    "models": [
        {
            "id": "swe-2-max",
            "name": "Devin SWE-2 Max",
            "url": "http://<SERVER_IP>:8080/v1/chat/completions",
            "toolCalling": true,
            "vision": true,
            "maxInputTokens": 128000,
            "maxOutputTokens": 16000
        }
    ]
}
```

### Giải thích field

| Field | Giá trị |
| --- | --- |
| `vendor` | Bắt buộc `"customendpoint"` |
| `apiKey` | Xem [mục 4](#4-apikey-hai-cách) |
| `apiType` | `"chat-completions"` \| `"responses"` \| `"messages"` — phải khớp `url` (bảng dưới) |
| `models[].id` | Model UID từ `GET /v1/models` (`swe-2-max`, `adaptive`, `claude-opus-5-5-max`, `gpt-6-astra-max`, …) |
| `models[].url` | **Full URL** tới endpoint — VS Code không tự nối path |
| `models[].apiType` | (optional) override `apiType` per-model — cho phép mix protocol trong cùng provider |
| `toolCalling`, `vision` | `true` — proxy hỗ trợ cả hai |

### apiType ↔ URL mapping

```jsonc
"apiType": "chat-completions"  →  "url": ".../v1/chat/completions"
"apiType": "responses"         →  "url": ".../v1/responses"
"apiType": "messages"          →  "url": ".../v1/messages"
```

Mix trong một provider (mỗi model có `apiType` + `url` riêng):

```json
"models": [
    { "id": "swe-2-max",            "name": "Devin (chat)",      "url": "http://<IP>:8080/v1/chat/completions", "apiType": "chat-completions", "toolCalling": true, "vision": true },
    { "id": "claude-opus-5-5-max",  "name": "Devin (messages)",  "url": "http://<IP>:8080/v1/messages",         "apiType": "messages",         "toolCalling": true, "vision": true },
    { "id": "adaptive",             "name": "Devin (responses)", "url": "http://<IP>:8080/v1/responses",        "apiType": "responses",        "toolCalling": true, "vision": true }
]
```

## 4. `apiKey` — hai cách

**a) Literal (test nhanh, debug dễ):**

```json
"apiKey": "sk-devin-5e9ef98957a458333d292ed21bd76861cc200bac"
```

**b) Secret reference (khuyến nghị — key không nằm trong file):**

```json
"apiKey": "${input:chat.lm.secret.-4ea7f8c1}"
```

`${input:chat.lm.secret.<id>}` là **tham chiếu** tới VS Code secret
storage, không phải key. Nó chỉ resolve được nếu secret đó **đã được tạo**:

- Cách tạo đúng: `Ctrl+Shift+P` → **Chat: Manage Language Models** →
  **Add Models → Custom Endpoint** → VS Code hỏi API key → paste
  `PROXY_API_KEY` → nó tự ghi `${input:...}` vào file.
- Copy `${input:...}` id từ máy/entry khác **không hoạt động** — secret
  storage là per-machine. Nếu id không tồn tại → VS Code gửi key
  rỗng/sai → proxy trả `Invalid API key` (401).

Giá trị cần nhập = `PROXY_API_KEY` trên server
(`cat .env` trong thư mục deploy, hoặc
`docker compose exec devin-to-api env | grep PROXY_API_KEY`).

## 5. Verify

1. Lưu file → `Ctrl+Shift+P` → **Developer: Reload Window** (model mới
   đôi khi không hiện ngay trong picker).
2. Chat view → model picker → chọn **Devin SWE-2 Max**.
3. Hỏi thử: `say hello`.

Test độc lập VS Code (chạy trên máy Windows, PowerShell):

```powershell
curl http://<SERVER_IP>:8080/healthz
curl http://<SERVER_IP>:8080/v1/chat/completions `
  -H "Authorization: Bearer <PROXY_API_KEY>" `
  -H "Content-Type: application/json" `
  -d '{"model":"swe-2-max","messages":[{"role":"user","content":"hi"}]}'
```

## 6. Troubleshooting

| Lỗi | Nguyên nhân | Fix |
| --- | --- | --- |
| `Invalid API key` (401) | Key sai — secret ref không resolve hoặc literal key gõ sai | Lấy lại key từ `.env` trên server; thử literal key trước |
| `Missing API key` | Không gửi header | apiKey rỗng — set lại |
| Request không tới server | `url` sai / port chặn | Mở `http://<IP>:8080/healthz` bằng browser — phải thấy JSON |
| 404 | `url` thiếu path | VS Code cần **full URL** (`.../v1/chat/completions`) |
| Model không có trong picker | File lỗi cú pháp hoặc cần reload | Validate JSON (phải là array), Reload Window |
| `model not found` | `id` không tồn tại upstream | `curl .../v1/models -H "Authorization: Bearer <key>"` lấy danh sách |
