# SMS Archive для OpenWrt

Служба копирует SMS с SIM и из памяти USB-модема в отдельные YAML-файлы на роутере. История и состояние синхронизации доступны в LuCI через **Система → Архив SMS**. Веб-страница читает локальную копию архива, поэтому она доступна при отключённом модеме. В разделе **SIM-карты** администратор может присвоить картам имена.

Целевое устройство первой версии — **Cudy TR3000 256MB v1** с **OpenWrt 25.12.6**, архитектура `aarch64_cortex-a53`, и **Fibocom L860‑GL‑16** на `/dev/ttyACM2`. Пакет MIT не включает настройки LTE.

## Установка и обновление

### Из подписанного feed

Подставьте владельца публичного репозитория. Скачайте публичный ключ из Releases и сравните его SHA‑256 с опубликованной контрольной суммой. После передачи ключа на Cudy добавьте feed один раз.

```sh
cp /tmp/sms-archive.pub.pem /etc/apk/keys/sms-archive.pub.pem
echo 'https://<owner>.github.io/openwrt-sms-archive/packages.adb' >> /etc/apk/repositories.d/customfeeds.list
apk update
apk add luci-app-sms-archive
```

Для обновления выполните `apk update` и `apk upgrade luci-app-sms-archive`. Выйдите и снова войдите в LuCI, если после установки меню ещё не появилось.

### Без доступа Cudy к интернету

Скачайте `luci-app-sms-archive-*-offline.tar.gz` и `SHA256SUMS` на компьютер. Проверьте контрольную сумму комплекта. Скопируйте комплект по SCP на Cudy в `/tmp/`, затем выполните команды на роутере.

```sh
mkdir -p /tmp/sms-archive-feed
tar -xzf /tmp/luci-app-sms-archive-0.1.0-r1-offline.tar.gz -C /tmp/sms-archive-feed
cp /tmp/sms-archive-feed/sms-archive.pub.pem /etc/apk/keys/sms-archive.pub.pem
apk --no-network --repositories-file /dev/null \
  --repository /tmp/sms-archive-feed/packages.adb \
  --repository /tmp/sms-archive-feed/kmods/packages.adb \
  add luci-app-sms-archive
```

В комплект входит `kmod-usb-acm` для штатного ядра этой прошивки. Библиотеки LuCI и поддержка USB уже присутствуют в заводском образе OpenWrt для Cudy. Замените имя tar-файла на имя скачанного выпуска.

## Импорт, очистка и диагностика

Служба запускается автоматически. Для незнакомой SIM первый успешный цикл копирует `SM` и `ME`, сверяет архив и сохраняет отметку. Начиная со следующего цикла служба может удалять подтверждённые записи с SIM. Записи из памяти модема `ME` только читаются. Сообщения `ME` без однозначной связи с SIM показываются как **SIM неизвестна**.

Лимит измеряется в занятых ячейках. Одна часть длинного SMS может занимать отдельную ячейку. По умолчанию после очистки остаётся до 10 ячеек, если есть подходящие записи. Белый список повышает приоритет сохранения на SIM, чёрный снижает. Оба списка влияют только на ротацию, все записи архивируются.

```sh
uci set sms_archive.settings.sim_keep_slots='10'
uci add_list sms_archive.settings.whitelist='MegaFon'
uci add_list sms_archive.settings.blacklist='RSCHS'
uci commit sms_archive
/etc/init.d/sms-archive restart
```

В LuCI видно время попытки, последнего успеха, состояние импорта и число ячеек. При сбое проверки записи удаление прекращается до следующего цикла. Для диагностики на роутере доступны команды:

```sh
/etc/init.d/sms-archive status
logread | grep sms-archive
```

Если нужно остановить опрос, выполните `/etc/init.d/sms-archive stop`. Чтобы отключить автозапуск, выполните `/etc/init.d/sms-archive disable`. Перед ручной работой с AT-портом остановите службу. Для разового импорта без удаления используйте `/usr/sbin/sms-archive -once -no-delete` при остановленной службе. Эта команда не включает автозапуск.

## Файлы и резервное копирование

Архив находится в `/root/sms-archive/messages/`. Файлы названы по UTC-времени и стабильному ID. В YAML записываются декодированные данные и исходный PDU каждой части. Чтение через LuCI не меняет записи в архиве.

Чтобы получить согласованную копию, остановите службу, затем с компьютера скопируйте архив и конфигурацию по SSH:

```sh
scp -O -r root@192.168.1.1:/root/sms-archive ./sms-archive-backup
scp -O root@192.168.1.1:/etc/config/sms_archive ./sms-archive-backup/uci-config
```

После копирования снова запустите `/etc/init.d/sms-archive start`. Обновление и удаление APK сохраняют архив и UCI. Перед перепрошивкой роутера храните отдельную проверенную копию.

## Сборка

Используйте Linux x86‑64 или Ubuntu WSL2. Нужны Go, OpenSSL, curl, rsync, zstd и стандартные зависимости OpenWrt SDK. Сборка загружает и проверяет SDK `25.12.6 mediatek/filogic`. Для повторной сборки можно указать уже распакованный SDK через `SMS_ARCHIVE_SDK_DIR`.

```sh
go test ./...
umask 077
openssl ecparam -name prime256v1 -genkey -noout -out "$HOME/sms-archive.key"
umask 022
SMS_ARCHIVE_SIGNING_KEY="$HOME/sms-archive.key" bash scripts/build-apk.sh
```

В `dist/feed/` появятся APK, публичный ключ и подписанный `packages.adb`. В `dist/release/` дополнительно появятся офлайн-комплект и контрольные суммы. Версия проекта следует SemVer, например `0.1.0` с Git-тегом `v0.1.0`. `SMS_ARCHIVE_VERSION` задаёт эту версию. `SMS_ARCHIVE_RELEASE` задаёт ревизию упаковки OpenWrt `rN`, которую можно повысить при повторной сборке той же версии. Один и тот же приватный ключ нужен для обновлений из этого feed. Храните его отдельно от исходников и архива SMS.

## Публикация в GitHub

1. Создайте публичный репозиторий `openwrt-sms-archive` и отправьте исходники. В настройках Pages выберите **GitHub Actions** как источник.
2. Для автоматического выпуска включите `ENABLE_CI_SIGNING=true`, задайте `APK_SIGNING_PUBLIC_SHA256` и сохраните `APK_SIGNING_KEY_PEM` как секрет окружения `package-signing`. Разрешите окружению ветку `main` для проверки и теги `v*` для выпуска.
3. Запустите **OpenWrt APK** вручную из `main`, чтобы проверить сборку и подпись. Проверка создаёт только временный артефакт с версией `0.0.0`. Чтобы выпустить новую версию, отправьте её SemVer-тег `vX.Y.Z`. Workflow соберёт APK, создаст GitHub Release и развернёт подписанный feed.

При локальной подписи отключите `ENABLE_CI_SIGNING`, приложите все файлы из `dist/release/` к GitHub Release и запустите **Publish signed APK feed**, передав тег релиза. Адрес feed этого проекта — `https://noob4ik.github.io/openwrt-sms-archive/packages.adb`.

Лицензия собственного кода и документации — MIT. Уведомления о сторонних Go-компонентах включаются в пакет при сборке.
