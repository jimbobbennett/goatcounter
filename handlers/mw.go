// Copyright © 2019 Martin Tournoij – This file is part of GoatCounter and
// published under the terms of a slightly modified EUPL v1.2 license, which can
// be found in the LICENSE file or at https://license.goatcounter.com

package handlers

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"zgo.at/goatcounter"
	"zgo.at/goatcounter/cfg"
	"zgo.at/goatcounter/cron"
	"zgo.at/guru"
	"zgo.at/json"
	"zgo.at/zdb"
	"zgo.at/zhttp"
	"zgo.at/zhttp/auth"
	"zgo.at/zlog"
)

var (
	redirect = func(w http.ResponseWriter, r *http.Request) error {
		zhttp.Flash(w, "Need to log in")
		return guru.Errorf(303, "/user/new")
	}

	loggedIn = auth.Filter(func(w http.ResponseWriter, r *http.Request) error {
		u := goatcounter.GetUser(r.Context())
		if u != nil && u.ID > 0 {
			return nil
		}
		return redirect(w, r)
	})

	loggedInOrPublic = auth.Filter(func(w http.ResponseWriter, r *http.Request) error {
		u := goatcounter.GetUser(r.Context())
		if (u != nil && u.ID > 0) || Site(r.Context()).Settings.Public {
			return nil
		}
		return redirect(w, r)
	})

	noSubSites = auth.Filter(func(w http.ResponseWriter, r *http.Request) error {
		if Site(r.Context()).Parent == nil ||
			*Site(r.Context()).Parent == 0 {
			return nil
		}
		zlog.FieldsRequest(r).Errorf("noSubSites")
		return guru.Errorf(403, "child sites can't access this")
	})

	adminOnly = auth.Filter(func(w http.ResponseWriter, r *http.Request) error {
		if Site(r.Context()).Admin() {
			return nil
		}
		return guru.Errorf(404, "")
	})

	keyAuth = auth.Add(func(ctx context.Context, key string) (auth.User, error) {
		u := &goatcounter.User{}
		err := u.ByTokenAndSite(ctx, key)
		return u, err
	})
)

type statusWriter interface{ Status() int }

func addctx(db zdb.DB, loadSite bool) func(http.Handler) http.Handler {
	started := goatcounter.Now()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			if r.URL.Path == "/status" {
				j, err := json.Marshal(map[string]string{
					"uptime":            goatcounter.Now().Sub(started).String(),
					"version":           cfg.Version,
					"last_persisted_at": cron.LastMemstore.Get().Format(time.RFC3339Nano),
				})
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)

				w.Write(j)
				return
			}

			// Add timeout on non-admin pages.
			t := 3
			switch {
			case strings.HasPrefix(r.URL.Path, "/admin"):
				t = 120
			case r.URL.Path == "/":
				t = 11
			}
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(r.Context(), time.Duration(t)*time.Second)
			defer func() {
				cancel()
				if ctx.Err() == context.DeadlineExceeded {
					if ww, ok := w.(statusWriter); !ok || ww.Status() == 0 {
						w.WriteHeader(http.StatusGatewayTimeout)
						w.Write([]byte("Server timed out"))
					}
				}
			}()

			// Add database.
			*r = *r.WithContext(zdb.With(ctx, db))
			if !cfg.Prod {
				if c, _ := r.Cookie("debug-explain"); c != nil {
					*r = *r.WithContext(zdb.With(ctx, zdb.NewExplainDB(db.(zdb.DBCloser), os.Stdout, c.Value)))
				}
			}

			// Load site from subdomain.
			if loadSite {
				var s goatcounter.Site
				err := s.ByHost(r.Context(), r.Host)

				// Special case so "http://localhost:8081" works: we don't
				// really need to bother with host match on dev if there's just
				// one site.
				if !cfg.Prod {
					var sites goatcounter.Sites
					err2 := sites.UnscopedList(r.Context())
					if err2 == nil && len(sites) == 1 {
						s = sites[0]
						err = nil
					}
				}

				if err != nil {
					if zdb.ErrNoRows(err) {
						err = guru.Errorf(400, "no site at this domain (%q)", r.Host)
					} else {
						zlog.FieldsRequest(r).Error(err)
					}

					zhttp.ErrPage(w, r, err)
					return
				}

				*r = *r.WithContext(goatcounter.WithSite(r.Context(), &s))
			}

			next.ServeHTTP(w, r)
		})
	}
}
