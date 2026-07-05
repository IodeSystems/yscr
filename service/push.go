package service

import (
	"encoding/json"
	"sync"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/iodesystems/yscr/config"
)

// pushHub holds the VAPID keypair + the set of browser push subscriptions and
// fans a notification out to all of them (Web Push). Subscriptions are kept in
// memory; a durable store is a later swap.
type pushHub struct {
	cfg  *config.Config
	mu   sync.Mutex
	subs map[string]*webpush.Subscription // keyed by endpoint
}

// newPushHub loads the VAPID keypair, generating + persisting one on first run.
func newPushHub(cfg *config.Config) (*pushHub, error) {
	if cfg.VAPID.Public == "" || cfg.VAPID.Private == "" {
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, err
		}
		cfg.VAPID.Private, cfg.VAPID.Public = priv, pub
		_ = cfg.Save() // best-effort persist so the key survives restarts
	}
	return &pushHub{cfg: cfg, subs: map[string]*webpush.Subscription{}}, nil
}

func (h *pushHub) add(sub *webpush.Subscription) {
	h.mu.Lock()
	h.subs[sub.Endpoint] = sub
	h.mu.Unlock()
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
			continue
		}
		sent++
	}
	return sent
}
