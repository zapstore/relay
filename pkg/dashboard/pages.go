package dashboard

import (
	"context"
	"net/http"
	"time"
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

	// rows come back DESC from the DB; reverse to ascending for the chart.
	n := len(rows)
	labels := make([]string, n)
	reqs := make([]int64, n)
	filters := make([]int64, n)
	events := make([]int64, n)
	var totalReqs, totalFilters, totalEvents int64
	for i, row := range rows {
		j := n - 1 - i
		labels[j] = row.Day
		reqs[j] = row.Reqs
		filters[j] = row.Filters
		events[j] = row.Events
		totalReqs += row.Reqs
		totalFilters += row.Filters
		totalEvents += row.Events
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
		Labels: labels,
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

func (d *T) appsPage(w http.ResponseWriter, r *http.Request) {
	if err := d.template.ExecuteTemplate(w, "apps", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (d *T) defenderPage(w http.ResponseWriter, r *http.Request) {
	if err := d.template.ExecuteTemplate(w, "defender", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
