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
                                      ├─ захват команды (raw PCM, без усиления)
                                      ├─ Athena Device Gateway /api/device/ws
                                      │  (ASR + ответ, без диалога Алисы)
                                      ├─ POST Athena /api/device/tts
                                      └─ POST Luna /api/audio/jobs (WAV)
```

Команда уходит в собственный Device Gateway Афины, а не в ASR Алисы. Поэтому
Афина не запускает и не отменяет штатный диалог, а нативное wake word «Алиса»
остаётся полностью штатным. При wake Афина показывает три разных состояния из
родных анимаций: слушает — зелёную `alice_listening.led`, думает — синюю
`alice_thinking.led`, отвечает — оранжевую `alice_speaking.led`. Яркость
слушания следует за микрофоном, яркость ответа — за громкостью TTS. Файлы
анимации только читаются и не изменяются. Если ядро ответило вопросом, но
прислало `listen=false`, приложение всё равно открывает окно follow-up, чтобы
можно было ответить без повторного wake word. Звуки wake/listen/error играются
тише штатных, TTS не приглушается.

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
   # dist/athena-assistant-0.2.5.yampkg
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
   adb push dist/athena-assistant-0.2.5.yampkg /data/local/tmp/
   adb shell "/data/yam/bin/yam-run install -y /data/local/tmp/athena-assistant-0.2.5.yampkg"
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

## Обучение слову «Афина»

Готовой модели в пакете нет: слово обучается на самой станции под её микрофоны,
канал `vqe_0`/`vqe_bnr` и акустику комнаты. Это важнее, чем готовая модель от
другого голоса: универсальная ONNX-модель из `models/` на записях этого
пользователя почти не срабатывала, поэтому она оставлена только как справочный
материал и в пакет не входит.

1. Откройте `http://<station>:8781/`.
2. Нажмите добавление слова и введите `Афина`.
3. Запишите 15–20 коротких фраз «Афина» с обычной громкостью, с разных
   расстояний и под разными углами к станции. Чем разнообразнее записи, тем
   меньше ложных срабатываний.
4. Запишите 8 секунд тишины/фона комнаты.
5. Если в комнате работает телевизор, радио или улица слышна, нажмите
   «Записать шум 2 минуты» и оставьте обычный фон. Это самый эффективный способ
   убрать реакцию на чужую речь. Отдельные ложные срабатывания тоже можно
   отмечать в UI — они попадут в обучение как негативы.
6. Запустите обучение и дождитесь появления модели.

Модель сохраняется в `plugin-state` и не перезаписывается при обновлении пакета.
Если позже появятся ложные срабатывания, отметьте их в UI как false и
переобучите слово: приложение использует эти записи как негативы.

Важно: если телевизор, радио или видео в комнате реально произносят слово
«Афина», приложение сработает — это не ошибка модели, а настоящее срабатывание
wake word. Штатная Алиса ведёт себя так же, когда из телевизора звучит
«Алиса». Для проверки ложных срабатываний используйте записи, где этого слова
нет.

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
| `POST` | `/api/enroll/noise?secs=120` | записать длинный шум/телевизор |
| `POST` | `/api/enroll/train` | обучить модель |

Полный набор маршрутов обучения и хитов — в UI и в `internal/daemon/api.go`.

## Безопасность и совместимость

- Не устанавливайте `yam/luna` поверх текущей Luna: это миграция data-dir и
  замена `S99luna`. Приложению это не нужно.
- Не меняйте `aliced.uniProxyUrl` и конфиг `quasar-bridge`.
- Плагин читает `vqe_0` как второй подписчик, не забирая поток у Quasar.
- Для Афины не вызывается `START_CONVERSATION` и не используется ASR Алисы:
  команда идёт через Athena Device Gateway. Штатный диалог не стартует и не
  отменяется.
- LED-матрица показывает одну из трёх инвертированных анимаций Алисы только на
  время цикла Афины (слушание, размышление, ответ) и гарантированно гасится
  после финального ответа, таймаута, ошибки или `utterance.drop`.

## Обновление

Повторная установка той же версии YAM требует удаления пакета:

```sh
adb shell "/data/yam/bin/yam-run remove athena/assistant"
adb shell "/data/yam/bin/yam-run install -y /data/local/tmp/athena-assistant-0.2.5.yampkg"
```

Затем `rescan` и `service/enable` (это и включает, и запускает плагин). Настройки и обученные слова лежат в
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
