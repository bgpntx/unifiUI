# UniFi UDR – Local GUI

Локальний веб-дашборд для моніторингу та управління UniFi Dream Router через Integration API v1.

## Можливості

- Перегляд сайтів, пристроїв, клієнтів
- WAN health графік (real-time download/upload)
- Фільтрація та пошук клієнтів/пристроїв
- Авторизація гостьових клієнтів (hotspot)
- Auto-refresh з налаштовуваним інтервалом
- Compact/Comfortable density режими

## Вимоги

- Go 1.22+ (для збірки)
- UniFi API Key (створюється в UDR → Settings → Integration)

## Збірка та запуск

```bash
cp .env.example .env
# Вписати UNIFI_API_KEY у .env

go build -o unifiui .
./unifiui
```

Відкрити http://localhost:5173

## Docker

```bash
docker build -t unifiui .
docker run -d --name unifiui \
  -e UNIFI_API_KEY=your-key \
  -e UDR_BASE=https://192.168.69.1 \
  -e UNSAFE_TLS=1 \
  -p 5173:5173 \
  unifiui
```

## Конфігурація

| Змінна | Обов'язково | Опис | Default |
|--------|-------------|------|---------|
| `UNIFI_API_KEY` | ✅ | API key з UDR | — |
| `UDR_BASE` | ❌ | URL роутера | `https://192.168.69.1` |
| `UNIFI_SITE_ID` | ❌ | UUID сайту | перший доступний |
| `PORT` | ❌ | Порт сервера | `5173` |
| `UNSAFE_TLS` | ❌ | Пропустити TLS валідацію | `0` |

## Архітектура

```
Browser ──fetch──▶ Go HTTP Server(:5173) ──X-API-KEY──▶ UniFi UDR
                   ├── /health
                   ├── /api/sites
                   ├── /api/clients
                   ├── /api/devices
                   ├── /api/wan/health
                   └── /api/clients/{id}/authorize
```

- API key зберігається тільки на сервері — браузер не має до нього доступу
- HTML/CSS/JS вбудовано в бінарник через `embed.FS`
- Zero зовнішніх залежностей (тільки стандартна бібліотека Go)

## Гарячі клавіші

| Клавіша | Дія |
|---------|-----|
| `R` | Оновити клієнтів і пристрої |
