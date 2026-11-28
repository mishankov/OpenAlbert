module openalbert/apps/backend

go 1.26.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/chromedp/cdproto v0.160.9
	github.com/chromedp/chromedp v0.24.0
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/google/uuid v1.6.0
	github.com/mishankov/OpenAlbert v0.0.0
	github.com/mishankov/OpenAlbert/shared/backup v0.0.0
	github.com/robfig/cron/v3 v3.0.1
	github.com/yuin/goldmark v1.8.7
	golang.org/x/crypto v0.59.0
	golang.org/x/net v0.63.0
	modernc.org/libc v1.82.14
	modernc.org/libz v0.23.0
	modernc.org/sqlite v1.63.0
)

require (
	filippo.io/age v1.3.2 // indirect
	filippo.io/hpke v0.4.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/go-json-experiment/json v0.0.0-20260213210345-44df1a37e875 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/minio/minio-go/v7 v7.4.1 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/tetratelabs/wazero v1.12.0 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.44.0 // indirect
	gopkg.in/ini.v1 v1.67.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.13.0 // indirect
)

replace (
	github.com/mishankov/OpenAlbert => ../..
	github.com/mishankov/OpenAlbert/shared/backup => ../../shared/backup
)
