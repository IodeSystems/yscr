package service

import (
	"context"
	"encoding/json"
	"sync"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/iodesystems/yscr/config"
	"github.com/iodesystems/yscr/store"
)

// pushHub holds the VAPID keypair + the set of browser push subscriptions and
// fans a notification out to all of them (Web Push). Subscriptions persist in
// Postgres when a store is wired (survive restarts); else in-memory only.
type pushHub struct {
	cfg  *config.Config
	db   *store.PG // optional durable subscription store (nil = in-memory)
	mu   sync.Mutex
	subs map[string]*webpush.Subscription // keyed by endpoint
}

// newPushHub loads the VAPID keypair (generating on first run) and any
// persisted subscriptions.
func newPushHub(cfg *config.Config, db *store.PG) (*pushHub, error) {
	if cfg.VAPID.Public == "" || cfg.VAPID.Private == "" {
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, err
		}
		cfg.VAPID.Private, cfg.VAPID.Public = priv, pub
		_ = cfg.Save() // best-effort persist so the key survives restarts
	}
	h := &pushHub{cfg: cfg, db: db, subs: map[string]*webpush.Subscription{}}
	if db != nil {
		if loaded, err := db.LoadSubscriptions(context.Background()); err == nil {
			for _, s := range loaded {
				h.subs[s.Endpoint] = &webpush.Subscription{
					Endpoint: s.Endpoint,
					Keys:     webpush.Keys{P256dh: s.P256dh, Auth: s.Auth},
				}
			}
		}
	}
	return h, nil
}

func (h *pushHub) add(sub *webpush.Subscription) {
	h.mu.Lock()
	h.subs[sub.Endpoint] = sub
	h.mu.Unlock()
	if h.db != nil {
		_ = h.db.SaveSubscription(context.Background(), store.PushSub{
			Endpoint: sub.Endpoint, P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth,
		})
	}
}

// notify sends a {title, body} push to every subscription, pruning any the
// push service reports gone (404/410). Returns how many were delivered.
func (h *pushHub) notify(title, body string) int {
	payload, _ := json.Marshal(map[string]string{"title": title, "body": body})

	h.mu.Lock()
	subs := make([]*webpush.Subscription, 0, len(h.subs))
	for _, s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	sent := 0
	for _, s := range subs {
		resp, err := webpush.SendNotification(payload, s, &webpush.Options{
			Subscriber:      h.cfg.VAPID.Subject,
			VAPIDPublicKey:  h.cfg.VAPID.Public,
			VAPIDPrivateKey: h.cfg.VAPID.Private,
			TTL:             30,
		})
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 404 || resp.StatusCode == 410 {
			h.mu.Lock()
			delete(h.subs, s.Endpoint)
			h.mu.Unlock()
			if h.db != nil {
				_ = h.db.DeleteSubscription(context.Background(), s.Endpoint)
			}
			continue
		}
		sent++
	}
	return sent
}
