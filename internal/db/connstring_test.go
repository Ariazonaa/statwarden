package db

import (
	"net/url"
	"testing"
)

// TestPortableConnString pins what is stripped before DATABASE_URL reaches a
// consumer other than pgxpool. The pool_* knobs are documented pgxpool
// settings, so an operator is entitled to put them in the URL — and both the
// migration runner (pgx.ParseConfig, which forwards unknown keys to the server
// as runtime parameters) and pg_dump (libpq, which rejects unknown URI
// parameters) then refused to start.
func TestPortableConnString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "url without pool params is untouched",
			in:   "postgres://u:p@db.example.test:5432/app?sslmode=disable",
			want: "postgres://u:p@db.example.test:5432/app?sslmode=disable",
		},
		{
			name: "url pool params stripped, others kept",
			in:   "postgres://u:p@db.example.test/app?pool_max_conns=20&sslmode=require&pool_health_check_period=1m",
			want: "postgres://u:p@db.example.test/app?sslmode=require",
		},
		{
			name: "pgx-only connection settings stripped too",
			in:   "postgresql://u@db.example.test/app?default_query_exec_mode=simple_protocol&statement_cache_capacity=0&application_name=sw",
			want: "postgresql://u@db.example.test/app?application_name=sw",
		},
		{
			name: "only pool params leaves no dangling question mark",
			in:   "postgres://u@db.example.test/app?pool_max_conns=5",
			want: "postgres://u@db.example.test/app",
		},
		{
			name: "keyword form",
			in:   "host=db.example.test pool_max_conns=10 user=u dbname=app pool_min_conns='2'",
			want: "host=db.example.test user=u dbname=app",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PortableConnString(tc.in)
			if got == tc.want {
				return
			}
			// A URL may legitimately come back with its query in another order;
			// compare it structurally before failing.
			gu, gerr := url.Parse(got)
			wu, werr := url.Parse(tc.want)
			if gerr == nil && werr == nil && gu.Scheme != "" &&
				gu.Host == wu.Host && gu.Path == wu.Path && gu.User.String() == wu.User.String() &&
				gu.Query().Encode() == wu.Query().Encode() {
				return
			}
			t.Errorf("PortableConnString(%q) = %q, want %q", tc.in, got, tc.want)
		})
	}
}
