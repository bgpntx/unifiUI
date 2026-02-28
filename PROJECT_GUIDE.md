# PROJECT_GUIDE.md — UniFi UDR Local GUI

## Огляд

Локальний веб-дашборд для моніторингу UniFi Dream Router. Express сервер виступає проксі до UDR Integration API v1, зберігаючи API key серверно.

## Структура

```
unifiUI/
├── server.js          # Express проксі-сервер
├── public/
│   └── index.html     # SPA дашборд (CSS+HTML+JS)
├── .env.example       # Шаблон конфігурації
├── package.json       # Залежності (express, node-fetch, cors)
└── README.md          # Документація
```

## Технічний стек

- **Runtime**: Node.js (ESM)
- **Backend**: Express 4
- **Frontend**: Vanilla HTML/CSS/JS (single file SPA)
- **API**: UniFi Integration v1 + Legacy stat/health

## Ключові рішення

- **Проксі-архітектура**: API key ніколи не потрапляє в браузер
- **Rate limiting**: 120 req/min per IP (in-memory)
- **CORS**: Обмежено до localhost
- **XSS захист**: Всі API дані escaped через `esc()` перед вставкою в DOM
- **HTTPS agent**: Синхронна ініціалізація, підтримка self-signed сертифікатів через `UNSAFE_TLS`
- **WAN графік**: Canvas-based, 60 точок (~1хв при 1Hz polling)

## API ендпоінти

| Метод | Шлях | Опис |
|-------|------|------|
| GET | `/health` | Health check з перевіркою UDR |
| GET | `/api/sites` | Список сайтів |
| POST | `/api/site` | Встановити дефолтний сайт |
| GET | `/api/clients` | Список клієнтів |
| GET | `/api/devices` | Список пристроїв |
| GET | `/api/wan/health` | WAN статус та швидкість |
| POST | `/api/clients/:id/authorize` | Авторизація гостя |

## Запуск

```bash
cp .env.example .env
# Заповнити UNIFI_API_KEY
npm install
npm start
```

## Останні зміни

- Виправлено race condition з HTTPS agent
- Додано rate limiting та CORS обмеження
- Додано request logging
- Виправлено XSS вразливість (HTML escaping)
- Покращено `/health` endpoint (перевірка з'єднання з UDR)
- Виправлено `.gitignore` (стандартний Node.js формат)
