# Üç binary de (api, worker, migrate) AYNI Dockerfile'dan üretiliyor:
# tek fark hangi cmd paketinin derlendiği (BINARY argümanı). Böylece derleme
# bayrakları, taban imaj ve güvenlik ayarları üç serviste de aynı kalıyor —
# üç ayrı dosyayı senkron tutma yükü yok.
#
#   docker build --build-arg BINARY=api ...

# --- derleme katmanı ---------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Önce sadece bağımlılık dosyaları: kaynak kod değiştiğinde bu katman
# önbellekten gelir, `go mod download` tekrar çalışmaz.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BINARY=api
# CGO_ENABLED=0: dinamik kütüphaneye ihtiyaç duymayan tamamen statik binary —
# scratch/alpine gibi minimal imajlarda çalışabilmesi için.
# -trimpath ve -ldflags="-s -w": yerel dosya yollarını ve hata ayıklama
# sembollerini çıkarır (küçük imaj + yerel dizin yapısını sızdırmama).
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${BINARY}

# --- çalıştırma katmanı ------------------------------------------------------
FROM alpine:3.22

# TLS sertifika kökleri: monitor kontrolleri (Faz 2.2) HTTPS adreslere istek
# atacak; kök sertifikalar olmadan her TLS bağlantısı hata verirdi.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 uptime

# root olarak çalıştırmıyoruz: konteyner ele geçirilse bile yetkisi sınırlı.
USER uptime

WORKDIR /app
COPY --from=build /out/app /app/app

ENTRYPOINT ["/app/app"]
