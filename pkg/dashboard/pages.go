package dashboard

import (
	"context"
	"net/http"
	"time"

	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/zapstore/defender/pkg/models"
	"github.com/zapstore/relay/pkg/analytics/store"
)

// ChartDataset represents a single dataset line in a chart.
type ChartDataset struct {
	Label           string  `json:"label"`
	Data            []int64 `json:"data"`
	BorderColor     string  `json:"borderColor"`
	BackgroundColor string  `json:"backgroundColor"`
}

// ChartData holds all data needed to render a chart component.
type ChartData struct {
	ID       string
	Title    string
	Labels   []string
	Datasets []ChartDataset
}

// CardData holds the data for a single metric card.
type CardData struct {
	Label string
	Value int64
}

// relayPageData holds the data passed to the relay metrics template.
type relayPageData struct {
	Cards []CardData
	Chart ChartData
}

func (d *T) relayPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	to := time.Now().Format("2006-01-02")
	from := time.Now().AddDate(0, 0, -30).Format("2006-01-02")

	rows, err := d.analytics.QueryRelayMetrics(ctx, from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	byDay := make(map[string]store.RelayMetrics, len(rows))
	for _, row := range rows {
		byDay[row.Day] = row
	}

	days := dayRange(from, to)
	reqs := make([]int64, len(days))
	filters := make([]int64, len(days))
	events := make([]int64, len(days))
	totalReqs, totalFilters, totalEvents := int64(0), int64(0), int64(0)

	for i, day := range days {
		if m, ok := byDay[day]; ok {
			reqs[i] = m.Reqs
			filters[i] = m.Filters
			events[i] = m.Events

			totalReqs += m.Reqs
			totalFilters += m.Filters
			totalEvents += m.Events
		}
	}

	data := relayPageData{
		Cards: []CardData{
			{Label: "Requests", Value: totalReqs},
			{Label: "Filters", Value: totalFilters},
			{Label: "Events", Value: totalEvents},
		},
	}
	data.Chart = ChartData{
		ID:     "relay-chart",
		Title:  "Daily Traffic",
		Labels: days,
		Datasets: []ChartDataset{
			{Label: "Requests", Data: reqs, BorderColor: "#6366f1", BackgroundColor: "rgba(99,102,241,0.08)"},
			{Label: "Filters", Data: filters, BorderColor: "#06b6d4", BackgroundColor: "rgba(6,182,212,0.08)"},
			{Label: "Events", Data: events, BorderColor: "#10b981", BackgroundColor: "rgba(16,185,129,0.08)"},
		},
	}

	if err := d.template.ExecuteTemplate(w, "relay", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type blossomPageData struct {
	Cards []CardData
	Chart ChartData
}

func (d *T) blossomPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	to := time.Now().Format("2006-01-02")
	from := time.Now().AddDate(0, 0, -30).Format("2006-01-02")

	rows, err := d.analytics.QueryBlossomMetrics(ctx, from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	byDay := make(map[string]store.BlossomMetrics, len(rows))
	for _, row := range rows {
		byDay[row.Day] = row
	}

	days := dayRange(from, to)
	checks := make([]int64, len(days))
	downloads := make([]int64, len(days))
	uploads := make([]int64, len(days))
	totalChecks, totalDownloads, totalUploads := int64(0), int64(0), int64(0)

	for i, day := range days {
		if m, ok := byDay[day]; ok {
			checks[i] = m.Checks
			downloads[i] = m.Downloads
			uploads[i] = m.Uploads

			totalChecks += m.Checks
			totalDownloads += m.Downloads
			totalUploads += m.Uploads
		}
	}

	data := blossomPageData{
		Cards: []CardData{
			{Label: "Checks", Value: totalChecks},
			{Label: "Downloads", Value: totalDownloads},
			{Label: "Uploads", Value: totalUploads},
		},
		Chart: ChartData{
			ID:     "blossom-chart",
			Title:  "Daily Traffic",
			Labels: days,
			Datasets: []ChartDataset{
				{Label: "Checks", Data: checks, BorderColor: "#f59e0b", BackgroundColor: "rgba(245,158,11,0.08)"},
				{Label: "Downloads", Data: downloads, BorderColor: "#ec4899", BackgroundColor: "rgba(236,72,153,0.08)"},
				{Label: "Uploads", Data: uploads, BorderColor: "#a78bfa", BackgroundColor: "rgba(167,139,250,0.08)"},
			},
		},
	}

	if err := d.template.ExecuteTemplate(w, "blossom", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (d *T) appsPage(w http.ResponseWriter, r *http.Request) {
	if err := d.template.ExecuteTemplate(w, "apps", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type defenderPageData struct {
	Policies []models.Policy
}

func (d *T) defenderPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	policies, err := d.defender.ListPolicies(ctx, "", "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for i := range policies {
		// convert hex nostr keys to npubs
		if policies[i].Entity.Platform == models.PlatformNostr {
			policies[i].Entity.ID, err = nip19.EncodePublicKey(policies[i].Entity.ID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	data := defenderPageData{Policies: policies}

	if err := d.template.ExecuteTemplate(w, "defender", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// dayRange returns every day from `from` to `to` inclusive in ascending order.
func dayRange(from, to string) []string {
	start, _ := time.Parse("2006-01-02", from)
	end, _ := time.Parse("2006-01-02", to)
	var days []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		days = append(days, d.Format("2006-01-02"))
	}
	return days
}
