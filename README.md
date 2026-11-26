# AthenaAssistantYAM

Приложение для Яндекс Станции 2, которое добавляет **Афину** рядом со штатной
Алисой. Оно ставится как пакет YAM v2 и работает как локальный плагин Luna.

- «Алиса» — как раньше: штатный Quasar, `quasar-bridge`, родные звуки и LED.
- «Афина» — своё wake word, распознавание через Luna, ответ через Athena Core,
  воспроизведение через аудио Luna.

Приложение **не перезаписывает** `quasar.cfg`, `quasar-bridge`, `S99luna`,
`spotter.json`, `local.sendspin` и другие рабочие файлы станции. Оно только
добавляет каталог плагина и id `athena.assistant` в списки Luna.

## Как это работает

```
vqe_0 (16 kHz) ──> AthenaAssistantYAM ──> «Афина»?
                                      │
                                      ├─ POST Luna /api/alice/recognize
                                      │  (тихое распознавание, без диалога Алисы)
                                      ├─ POST Athena /api/device/ask
                                      ├─ POST Athena /api/device/tts
                                      └─ POST Luna /api/audio/jobs (WAV)
```

`/api/alice/recognize` — это режим распознавания без запуска облачного диалога
Алисы. Поэтому Афина не перебивает Алису и не отменяет её запросы. Нативное
wake word «Алиса» остаётся полностью штатным.

## Требования

- Яндекс Станция 2 (`yandexstation_2`), aarch64.
- Установленный YAM v2 в `/data/yam`.
- Luna, запущенная как локальный сервис на `:8099`.
- Athena Core с device API, например `http://192.168.0.56:8000/api`.
- Device-токен `dev_...` для endpoint вида `yandexstation`.

## Установка

1. Соберите пакет на Linux или в Docker:

   ```sh
   ./package.sh
   # dist/athena-assistant-0.2.0.yampkg
   ```

   Для сборки нужны Go и Python 3 с `zstandard`. На Windows удобно собрать
   бинарник в Docker:

   ```sh
   docker run --rm -v "$PWD:/src" -w /src \
     -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=arm64 \
     golang:1.24-alpine go build -trimpath -ldflags "-s -w" -o dist/athena ./cmd/athena
   python scripts/pack.py
   ```

2. Скопируйте пакет на станцию и установите:

   ```sh
   adb push dist/athena-assistant-0.2.0.yampkg /data/local/tmp/
   adb shell "/data/yam/bin/yam-run install -y /data/local/tmp/athena-assistant-0.2.0.yampkg"
   ```

   Postinstall только копирует плагин в
   `/data/quazar/luna/data/plugins/athena.assistant` и добавляет id в
   `enabled.json`/`services.json`. Luna при этом не перезапускается.

3. Подхватите плагин через API Luna:

   ```sh
   curl -X POST -H 'Content-Type: application/json' -d '{}' \
     http://192.168.0.110:8099/api/plugins/rescan
   curl -X POST -H 'Content-Type: application/json' -d '{}' \
     http://192.168.0.110:8099/api/plugins/athena.assistant/service/enable
   ```

4. Откройте UI приложения:

   ```
   http://192.168.0.110:8781/
   ```

   Укажите URL Athena Core (с `/api`), device-токен и, при необходимости, URL
   Luna (`http://127.0.0.1:8099`). Нажмите «Сохранить», затем «Проверить ответ» —
   колонка должна произнести ответ Афины.

## Готовое слово «Афина»

В пакет уже входит готовая модель wake word «Афина» (ONNX, сконвертированная
в родной формат приложения). При первом запуске она автоматически копируется в
`words/афина/model.json`, слово включается, и обучение вручную не требуется.

Если готовое слово срабатывает нестабильно под ваш голос и микрофоны станции,
его можно переобучить на месте:

1. Откройте `http://<station>:8781/`.
2. Нажмите добавление слова и введите `Афина` (или откройте существующее).
3. Запишите 6–10 коротких фраз «Афина» с обычной громкостью и расстояния.
4. Запишите 8 секунд тишины/фона комнаты.
5. Запустите обучение и дождитесь появления модели.

Готовая модель не перезаписывается автоматически: повторная установка пакета
не трогает уже обученное слово.

После этого скажите «Афина», пауза — и команду, например:

```
Афина, который час?
Афина, включи свет в гостиной.
```

Чувствительность слова настраивается в UI. Если слишком много ложных
срабатываний — уменьшите её; если слово не ловится — увеличьте.

## API приложения

Порт `8781`, без отдельной авторизации (как у Luna в локальной сети).

| Метод | Путь | Назначение |
|---|---|---|
| `GET` | `/api/state` | состояние микрофона, слова, хиты |
| `POST` | `/api/listen` | один цикл «распознать → Афина → ответ» |
| `GET/POST` | `/api/athena/config` | URL, токен (только запись), таймаут |
| `GET` | `/api/athena/health` | проверка Athena и Luna |
| `POST` | `/api/athena/ask` | текстовый тест без микрофона |
| `POST` | `/api/enroll/start` | начать обучение `{ "name": "Афина" }` |
| `POST` | `/api/enroll/take` | записать один пример |
| `POST` | `/api/enroll/ambient` | записать фон |
| `POST` | `/api/enroll/train` | обучить модель |

Полный набор маршрутов обучения и хитов — в UI и в `internal/daemon/api.go`.

## Безопасность и совместимость

- Не устанавливайте `yam/luna` поверх текущей Luna: это миграция data-dir и
  замена `S99luna`. Приложению это не нужно.
- Не меняйте `aliced.uniProxyUrl` и конфиг `quasar-bridge`.
- Плагин читает `vqe_0` как второй подписчик, не забирая поток у Quasar.
- Для Афины не вызывается `START_CONVERSATION`; используется только
  `recognize`, поэтому штатный диалог Алисы не стартует и не отменяется.
- LED-кольцо подсвечивается синим только на время цикла Афины и затем
  возвращается в штатный режим Luna.

## Обновление

Повторная установка той же версии YAM требует удаления пакета:

```sh
adb shell "/data/yam/bin/yam-run remove athena/assistant"
adb shell "/data/yam/bin/yam-run install -y /data/local/tmp/athena-assistant-0.2.0.yampkg"
```

Затем `rescan` и `service/start`. Настройки и обученные слова лежат в
`/data/quazar/luna/data/plugins/plugin-state/athena.assistant/` и при
переустановке пакета не удаляются.

## Удаление

```sh
curl -X POST -H 'Content-Type: application/json' -d '{}' \
  http://192.168.0.110:8099/api/plugins/athena.assistant/service/stop
adb shell "/data/yam/bin/yam-run remove athena/assistant"
adb shell "rm -rf /data/quazar/luna/data/plugins/athena.assistant"
```

Затем уберите `athena.assistant` из `enabled.json` и `services.json` (или
восстановите `*.athena.bak`, если с момента установки списки не менялись).
Обученные слова можно оставить или удалить вместе с `plugin-state`.

## Диагностика

```sh
# плагин виден Luna и запущен
curl http://192.168.0.110:8099/api/plugins

# логи процесса
curl http://192.168.0.110:8099/api/plugins/athena.assistant/logs

# проверка Athena
curl http://192.168.0.110:8781/api/athena/health
```

Если Luna пишет `using script ... without executable permission`, проверьте
`ls -l /data/quazar/luna/data/plugins/athena.assistant/bin/athena`: файл должен
быть `0755`. Postinstall выставляет это сам.

## Лицензия

Apache-2.0. Проект использует наработки station-wakeword; подробности в
`NOTICE`. Пакет не связан с Яндексом.
