package db

import (
	"net/url"
	"regexp"
	"strings"
)

// pgxOnlyParams are connection-string keys that pgx/pgxpool consume themselves
// and nothing else understands. pgxpool removes them before connecting; every
// other consumer of DATABASE_URL does not.
var pgxOnlyParams = map[string]bool{
	"statement_cache_capacity":   true,
	"description_cache_capacity": true,
	"default_query_exec_mode":    true,
}

func isPgxOnlyParam(key string) bool {
	return strings.HasPrefix(key, "pool_") || pgxOnlyParams[key]
}

// keywordParam matches one key=value pair of a keyword/value connection string,
// with the value either single-quoted or a run of non-space characters.
var keywordParam = regexp.MustCompile(`(^|\s+)([a-z_]+)=('(?:[^'\\]|\\.)*'|\S*)`)

// PortableConnString returns databaseURL without the pgxpool/pgx-only settings
// (pool_* and a few pgx connection knobs), for every consumer that is not the
// pgxpool itself: the migration runner and pg_dump.
//
// Those settings are documented pgxpool options, so an operator tuning the pool
// is entitled to put pool_max_conns in DATABASE_URL. The pool honoured it, and
// then migrations failed at startup — pgx.ParseConfig forwards unknown keys to
// the server as runtime parameters, which rejects them — and pg_dump refused the
// URI outright. Both URL and keyword/value forms are handled; anything that does
// not parse is returned unchanged, so the consumer reports its own error.
func PortableConnString(databaseURL string) string {
	if strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://") {
		u, err := url.Parse(databaseURL)
		if err != nil {
			return databaseURL
		}
		q := u.Query()
		changed := false
		for key := range q {
			if isPgxOnlyParam(key) {
				q.Del(key)
				changed = true
			}
		}
		if !changed {
			return databaseURL
		}
		u.RawQuery = q.Encode()
		return u.String()
	}
	out := keywordParam.ReplaceAllStringFunc(databaseURL, func(m string) string {
		sub := keywordParam.FindStringSubmatch(m)
		if isPgxOnlyParam(sub[2]) {
			return ""
		}
		return m
	})
	return strings.TrimSpace(out)
}
