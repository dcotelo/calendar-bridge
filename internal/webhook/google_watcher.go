package webhook

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	calendar "google.golang.org/api/calendar/v3"
)

// googleWatcher implements ChannelWatcher using the Google Calendar
// events.watch / channels.stop APIs.
//
// It holds one *calendar.Service per account, since a watch channel is
// registered on a specific account's calendar and must be stopped through the
// same authenticated service.
type googleWatcher struct {
	services map[string]*calendar.Service // account name -> service
}

// NewGoogleWatcher builds a ChannelWatcher over per-account Calendar services.
// The map key must match the account name passed to Watch/Stop.
func NewGoogleWatcher(services map[string]*calendar.Service) ChannelWatcher {
	return &googleWatcher{services: services}
}

func (g *googleWatcher) Watch(ctx context.Context, account, calendarID, callbackURL, token string, ttl time.Duration) (Channel, error) {
	svc, ok := g.services[account]
	if !ok {
		return Channel{}, fmt.Errorf("no calendar service for account %q", account)
	}

	channelID := uuid.NewString()
	req := &calendar.Channel{
		Id:      channelID,
		Type:    "web_hook",
		Address: callbackURL,
		Token:   token,
	}
	if ttl > 0 {
		// Google expects an absolute expiration in epoch milliseconds.
		req.Expiration = time.Now().Add(ttl).UnixMilli()
	}

	res, err := svc.Events.Watch(calendarID, req).Context(ctx).Do()
	if err != nil {
		return Channel{}, fmt.Errorf("events.watch on %s/%s: %w", account, calendarID, err)
	}

	expiry := time.Now().Add(ttl)
	if res.Expiration != 0 {
		expiry = time.UnixMilli(res.Expiration)
	}
	return Channel{
		ID:         res.Id,
		ResourceID: res.ResourceId,
		Account:    account,
		CalendarID: calendarID,
		Expiry:     expiry,
	}, nil
}

func (g *googleWatcher) Stop(ctx context.Context, ch Channel) error {
	svc, ok := g.services[ch.Account]
	if !ok {
		return fmt.Errorf("no calendar service for account %q", ch.Account)
	}
	return svc.Channels.Stop(&calendar.Channel{
		Id:         ch.ID,
		ResourceId: ch.ResourceID,
	}).Context(ctx).Do()
}
