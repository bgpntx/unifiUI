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

- Node.js >= 18
- UniFi API Key (створюється в UDR → Settings → Integration)

## Встановлення

```bash
cp .env.example .env
# Вписати UNIFI_API_KEY у .env

npm install
npm start
```

Відкрити http://localhost:5173

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
Browser ──fetch──▶ Express(:5173) ──X-API-KEY──▶ UniFi UDR
                   ├── /api/sites
                   ├── /api/clients
                   ├── /api/devices
                   ├── /api/wan/health
                   └── /health
```

API key зберігається тільки на сервері — браузер не має до нього доступу.

## Гарячі клавіші

| Клавіша | Дія |
|---------|-----|
| `R` | Оновити клієнтів і пристрої |
