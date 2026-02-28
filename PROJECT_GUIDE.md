# PROJECT_GUIDE.md — UniFi UDR Local GUI

## Огляд

Локальний веб-дашборд для моніторингу UniFi Dream Router. Go HTTP сервер виступає проксі до UDR Integration API v1, зберігаючи API key серверно. HTML/CSS/JS вбудовано в бінарник через `embed.FS`.

## Структура

```
unifiUI/
├── main.go            # Go HTTP сервер (API proxy, middleware, embed)
├── go.mod             # Go module
├── public/
│   └── index.html     # SPA дашборд (CSS+HTML+JS)
├── Dockerfile         # Multi-stage: golang:1.22-alpine → scratch
├── .env.example       # Шаблон конфігурації
├── README.md          # Документація
└── PROJECT_GUIDE.md   # Цей файл
```

## Технічний стек

- **Мова**: Go 1.22+
- **Залежності**: Zero (тільки stdlib)
- **Frontend**: Vanilla HTML/CSS/JS (embedded через `embed.FS`)
- **API**: UniFi Integration v1 + Legacy stat/health
- **Docker**: scratch base, ~5 МБ образ

## Ключові рішення

- **Проксі-архітектура**: API key ніколи не потрапляє в браузер
- **embed.FS**: HTML вбудований в бінарник, один файл для деплою
- **Rate limiting**: 120 req/min per IP (in-memory, sync.Mutex)
- **CORS**: Обмежено до localhost
- **XSS захист**: Всі API дані escaped через `esc()` на фронтенді
- **TLS**: Підтримка self-signed сертифікатів через `UNSAFE_TLS`
- **WAN графік**: Canvas-based, 60 точок (~1хв при 1Hz polling)
- **Multi-WAN**: Per-WAN трафік (rx/tx) з gateway device stats + назви з Integration v1 + WAN selector (Combined/WAN1/WAN2)
- **Thread-safe site ID**: `sync.RWMutex` для мутабельного default site
- **Graceful shutdown**: SIGINT/SIGTERM → 5s timeout

## API ендпоінти

| Метод | Шлях | Опис |
|-------|------|------|
| GET | `/health` | Health check з перевіркою UDR |
| GET | `/api/sites` | Список сайтів |
| POST | `/api/site` | Встановити дефолтний сайт |
| GET | `/api/clients` | Список клієнтів |
| GET | `/api/devices` | Список пристроїв |
| GET | `/api/wan/health` | WAN статус, трафік, per-WAN latency |
| GET | `/api/wan/raw` | Debug: raw WAN дані з обох API |
| POST | `/api/clients/{id}/authorize` | Авторизація гостя |

## Збірка та запуск

```bash
# Локально
go build -o unifiui .
UNIFI_API_KEY=xxx ./unifiui

# Docker
docker build -t unifiui .
docker run -e UNIFI_API_KEY=xxx -p 5173:5173 unifiui
```

## Останні зміни

- Повний рефакторинг з Node.js/Express на Go
- Zero зовнішніх залежностей
- Додано Dockerfile (multi-stage, scratch)
- Вбудований healthcheck для Docker
- Graceful shutdown
- Multi-WAN: aggregate трафік + per-WAN latency + назви з Integration v1
