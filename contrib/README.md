# Luna: исправление цветов LED на Station 2

На Яндекс Станции 2 подсветка — это матрица 89 пикселей в `/dev/fb0` с форматом **RGBW32**.
Четвёртый байт пикселя — белый канал, а не альфа.

Luna 0.2.39 писала пиксели как `B, G, R, 0xFF`:

- красный и синий менялись местами;
- белый канал всегда был 255, поэтому любой цвет выглядел белым.

Патч `luna-station2-rgbw.patch` меняет кодирование на `R, G, B, 0x00`.
Он не трогает прошивку, `quasar.cfg`, `quasar-bridge`, нативные модели и остальные
плагины Luna.

## Применить к исходникам Luna 0.2.39

```sh
git clone https://github.com/nyakokitsu/luna
cd luna
git checkout b631380
git am /path/to/luna-station2-rgbw.patch
```

## Собрать для Station 2

```sh
docker run --rm -v "$PWD:/src" -w /src \
  ghcr.io/rust-cross/cargo-zigbuild:latest \
  cargo zigbuild --locked --release \
  --target aarch64-unknown-linux-musl \
  --manifest-path stations/yandexstation_2/Cargo.toml \
  --bin luna-yandexstation_2
```

Готовый файл: `target/aarch64-unknown-linux-musl/release/luna-yandexstation_2`.

## Установка на станцию

```sh
adb push target/aarch64-unknown-linux-musl/release/luna-yandexstation_2 /data/local/tmp/luna-patched
adb shell "cp -a /data/quazar/luna/bin/luna /data/quazar/luna/bin/luna.backup"
adb shell "cp /data/local/tmp/luna-patched /data/quazar/luna/bin/luna && chmod 0755 /data/quazar/luna/bin/luna"
adb shell "/etc/init.d/S99luna restart"
```

Проверка: `POST /api/led/ring` с `{"colors":["#FF0000"]}`.
В `/dev/fb0` должно быть `ff 00 00 00`, а матрица — красной.

Откат: вернуть `luna.backup` на место `luna` и перезапустить `S99luna`.
