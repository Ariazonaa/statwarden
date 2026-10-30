module github.com/Ariazonaa/statwarden

go 1.26.10

// Raised for security, not for a language feature, twice now. 1.26.5 → 1.26.6
// for GO-2026-5026, -5972, -6088, -6089, -6090, -6091 and -6218; 1.26.6 →
// 1.26.10 for twelve more REACHABLE standard-library advisories (GO-2026-6599
// through -6809: net/http, net/textproto, crypto/tls, crypto/x509,
// html/template), several of them fixed only in 1.26.9 or 1.26.10. govulncheck
// fails CI on reachable findings. Nothing in this module changed; the
// vulnerability database did. The Dockerfile's build image is pinned to the
// same patch release and has to move with this line.
//
// The `go` directive rather than a `toolchain` one on purpose: actions/setup-go
// reads this line and installs the matching release from go.dev/dl, whereas a
// toolchain switch fetches through the module proxy — which is one more thing
// that can be unavailable at the moment CI is already red.

require (
	github.com/SherClockHolmes/webpush-go v1.4.0
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/go-chi/chi/v5 v5.5.0
	github.com/go-pdf/fpdf v0.9.0
	github.com/go-sql-driver/mysql v1.10.1
	github.com/golang-migrate/migrate/v4 v4.20.1
	github.com/gorilla/websocket v1.5.3
	github.com/gosnmp/gosnmp v1.47.0
	github.com/jackc/pgx/v5 v5.11.0
	github.com/miekg/dns v1.1.73
	github.com/prometheus-community/pro-bing v0.9.1
	github.com/prometheus/client_golang v1.25.0
	github.com/redis/go-redis/v9 v9.23.0
	github.com/tidwall/gjson v1.20.0
	go.mongodb.org/mongo-driver v1.17.10
	golang.org/x/crypto v0.58.0
	golang.org/x/image v0.47.0
	golang.org/x/sync v0.24.0
	google.golang.org/grpc v1.85.0
	gopkg.in/yaml.v3 v3.0.1
	layeh.com/radius v0.0.0-20231213012653-1006025d24f8
	modernc.org/sqlite v1.61.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/golang/snappy v0.0.4 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgerrcode v0.0.0-20220416144525-469b46aa5efa // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.20.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/montanaflynn/stats v0.7.1 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.1.2 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
