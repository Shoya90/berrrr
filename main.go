package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const (
	apiBase    = "https://v6.vbb.transport.rest"
	configFile = ".commute_favorites.json"
)

// Station represents a transit station
type Station struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// Config stores user preferences
type Config struct {
	Routes     []FavoriteRoute `json:"routes"`
	LastOrigin Station         `json:"last_origin"`
	LastDest   Station         `json:"last_dest"`
}

// FavoriteRoute stores a saved route
type FavoriteRoute struct {
	Origin Station `json:"origin"`
	Dest   Station `json:"dest"`
}

// Leg represents a single transit leg
type Leg struct {
	Line          string
	Type          string
	Product       string
	From          string
	To            string
	Departure     time.Time
	Arrival       time.Time
	WaitBefore    time.Duration
	DepDelay      int
	ArrDelay      int
	Occupancy     string
	ServiceStatus []string
	DepPlatform   string
	ArrPlatform   string
	Cycle         int
	LineColor     string
	TripID        string
}

// Journey represents a complete journey with multiple legs
type Journey struct {
	LeaveAt   time.Time
	ArriveAt  time.Time
	Duration  time.Duration
	TotalWait time.Duration
	Legs      []Leg
	IsNew     bool
}

// DelayHistory tracks delay trends for sparklines
type DelayHistory struct {
	Line    string
	Delays  []int
	Updated time.Time
}

// API Response types
type APILocation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type APILine struct {
	Name    string `json:"name"`
	Product string `json:"product"`
	Color   struct {
		FG string `json:"fg"`
		BG string `json:"bg"`
	} `json:"color"`
}

type APIRemark struct {
	Type string `json:"type"`
	Code string `json:"code"`
	Text string `json:"text"`
}

type APILeg struct {
	Origin                   *APILocation `json:"origin"`
	Destination              *APILocation `json:"destination"`
	Departure                string       `json:"departure"`
	Arrival                  string       `json:"arrival"`
	Line                     *APILine     `json:"line"`
	DepartureDelay           *int         `json:"departureDelay"`
	ArrivalDelay             *int         `json:"arrivalDelay"`
	DeparturePlatform        string       `json:"departurePlatform"`
	PlannedDeparturePlatform string       `json:"plannedDeparturePlatform"`
	ArrivalPlatform          string       `json:"arrivalPlatform"`
	PlannedArrivalPlatform   string       `json:"plannedArrivalPlatform"`
	Remarks                  []APIRemark  `json:"remarks"`
	TripId                   string       `json:"tripId"`
	Cycle                    *struct {
		Min int `json:"min"`
	} `json:"cycle"`
}

type APIJourney struct {
	Legs []APILeg `json:"legs"`
}

type APIJourneysResponse struct {
	Journeys []APIJourney `json:"journeys"`
}

var defaultHome = Station{ID: "900180001", Name: "S Köpenick (Berlin)"}
var defaultWork = Station{ID: "900100041", Name: "Brunnenstr./Invalidenstr. (Berlin)"}

// Spinner frames for loading animation
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Transport type colors
var productColors = map[string]tcell.Color{
	"suburban": tcell.ColorGreen,
	"subway":   tcell.ColorBlue,
	"tram":     tcell.ColorRed,
	"bus":      tcell.ColorPurple,
	"ferry":    tcell.ColorTeal,
	"regional": tcell.ColorYellow,
	"express":  tcell.ColorYellow,
}

func getProductColor(product string) string {
	colors := map[string]string{
		"suburban": "green",
		"subway":   "blue",
		"tram":     "red",
		"bus":      "purple",
		"ferry":    "teal",
		"regional": "yellow",
		"express":  "yellow",
	}
	if c, ok := colors[product]; ok {
		return c
	}
	return "white"
}

func getProductIcon(product string) string {
	icons := map[string]string{
		"suburban": "[S]",
		"subway":   "[U]",
		"tram":     "[T]",
		"bus":      "[B]",
		"ferry":    "[F]",
		"regional": "[R]",
		"express":  "[I]",
	}
	if icon, ok := icons[product]; ok {
		return icon
	}
	return "[ ]"
}

func cleanStation(name string) string {
	re1 := regexp.MustCompile(`\s*\[.*?\]`)
	re2 := regexp.MustCompile(`\s*\(Berlin\)`)
	re3 := regexp.MustCompile(`^S\+U\s+`)
	re4 := regexp.MustCompile(`^S\s+`)
	re5 := regexp.MustCompile(`^U\s+`)

	name = re1.ReplaceAllString(name, "")
	name = re2.ReplaceAllString(name, "")
	name = re3.ReplaceAllString(name, "")
	name = re4.ReplaceAllString(name, "")
	name = re5.ReplaceAllString(name, "")
	name = strings.ReplaceAll(name, " Bhf", "")
	if idx := strings.Index(name, "/"); idx != -1 {
		name = name[:idx]
	}
	return strings.TrimSpace(name)
}

func parseTime(isoString string) (time.Time, error) {
	if isoString == "" {
		return time.Time{}, fmt.Errorf("empty time string")
	}
	isoString = strings.ReplaceAll(isoString, "Z", "+00:00")
	return time.Parse(time.RFC3339, isoString)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return t.Format("15:04")
}

// formatCountdown formats duration as countdown with color
func formatCountdown(d time.Duration) string {
	if d < 0 {
		return "[red::b]GONE[-:-:-]"
	}
	mins := int(d.Minutes())
	secs := int(d.Seconds()) % 60
	if mins < 1 {
		return fmt.Sprintf("[red::b]%ds[-:-:-]", secs)
	} else if mins < 5 {
		return fmt.Sprintf("[yellow]%d:%02d[-]", mins, secs)
	}
	return fmt.Sprintf("[green]%d:%02d[-]", mins, secs)
}

// sparkline generates a mini graph from delay values
func sparkline(values []int, width int) string {
	if len(values) == 0 {
		return strings.Repeat("▁", width)
	}

	blocks := []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

	minVal, maxVal := values[0], values[0]
	for _, v := range values {
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
	}

	result := ""
	step := len(values) / width
	if step < 1 {
		step = 1
	}

	for i := 0; i < width && i*step < len(values); i++ {
		v := values[i*step]
		idx := 0
		if maxVal > minVal {
			idx = int(float64(v-minVal) / float64(maxVal-minVal) * 7)
		}
		if idx > 7 {
			idx = 7
		}
		result += string(blocks[idx])
	}

	for len(result) < width {
		result += "▁"
	}

	return result
}

// occupancyBar generates static occupancy display
func occupancyBar(level string, frame int) string {
	switch level {
	case "low":
		return "[green]▓░░░░[-]"
	case "medium":
		return "[yellow]▓▓▓░░[-]"
	case "high":
		return "[red]▓▓▓▓▓[-]"
	default:
		return "[dim]░░░░░[-]"
	}
}

func getConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, configFile)
}

func loadConfig() Config {
	config := Config{
		LastOrigin: defaultHome,
		LastDest:   defaultWork,
	}

	data, err := os.ReadFile(getConfigPath())
	if err != nil {
		return config
	}

	json.Unmarshal(data, &config)
	return config
}

func saveConfig(config Config) {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(getConfigPath(), data, 0644)
}

func searchStations(query string) ([]Station, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("results", "10")

	resp, err := http.Get(fmt.Sprintf("%s/locations?%s", apiBase, params.Encode()))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var locations []APILocation
	if err := json.Unmarshal(body, &locations); err != nil {
		return nil, err
	}

	var stations []Station
	for _, loc := range locations {
		if loc.Type == "stop" {
			stations = append(stations, Station{
				ID:   loc.ID,
				Name: loc.Name,
				Type: loc.Type,
			})
		}
	}
	return stations, nil
}

func parseOccupancy(remarks []APIRemark) string {
	for _, r := range remarks {
		code := strings.ToLower(r.Code)
		text := strings.ToLower(r.Text)
		if strings.Contains(code, "occup") || strings.Contains(text, "occupancy") {
			if strings.Contains(text, "low") {
				return "low"
			} else if strings.Contains(text, "medium") || strings.Contains(text, "moderate") {
				return "medium"
			} else if strings.Contains(text, "high") {
				return "high"
			}
		}
	}
	return ""
}

func parseServiceStatus(remarks []APIRemark) []string {
	var statuses []string
	for _, r := range remarks {
		if r.Type == "warning" || r.Type == "status" {
			if r.Text != "" {
				statuses = append(statuses, r.Text)
			}
		}
	}
	return statuses
}

func fetchJourneys(originID, destID string, filters map[string]bool) ([]Journey, error) {
	params := url.Values{}
	params.Set("from", originID)
	params.Set("to", destID)
	params.Set("transfers", "3")
	params.Set("results", "25")
	params.Set("remarks", "true")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("%s/journeys?%s", apiBase, params.Encode()))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var apiResp APIJourneysResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, err
	}

	var journeys []Journey

	for _, aj := range apiResp.Journeys {
		if len(aj.Legs) == 0 {
			continue
		}

		var legs []Leg
		var totalWait time.Duration
		var prevArrival time.Time

		for _, al := range aj.Legs {
			if al.Line == nil {
				if arr, err := parseTime(al.Arrival); err == nil {
					prevArrival = arr
				}
				continue
			}

			dep, err := parseTime(al.Departure)
			if err != nil {
				continue
			}
			arr, err := parseTime(al.Arrival)
			if err != nil {
				continue
			}

			var wait time.Duration
			if !prevArrival.IsZero() && dep.After(prevArrival) {
				wait = dep.Sub(prevArrival)
				totalWait += wait
			}

			originName := ""
			if al.Origin != nil {
				originName = al.Origin.Name
			}
			destName := ""
			if al.Destination != nil {
				destName = al.Destination.Name
			}

			depDelay := 0
			if al.DepartureDelay != nil {
				depDelay = *al.DepartureDelay
			}
			arrDelay := 0
			if al.ArrivalDelay != nil {
				arrDelay = *al.ArrivalDelay
			}

			depPlatform := al.DeparturePlatform
			if depPlatform == "" {
				depPlatform = al.PlannedDeparturePlatform
			}
			arrPlatform := al.ArrivalPlatform
			if arrPlatform == "" {
				arrPlatform = al.PlannedArrivalPlatform
			}

			cycle := 0
			if al.Cycle != nil {
				cycle = al.Cycle.Min / 60
			}

			lineColor := ""
			if al.Line.Color.BG != "" {
				lineColor = al.Line.Color.BG
			}

			leg := Leg{
				Line:          al.Line.Name,
				Product:       al.Line.Product,
				From:          originName,
				To:            destName,
				Departure:     dep,
				Arrival:       arr,
				WaitBefore:    wait,
				DepDelay:      depDelay,
				ArrDelay:      arrDelay,
				Occupancy:     parseOccupancy(al.Remarks),
				ServiceStatus: parseServiceStatus(al.Remarks),
				DepPlatform:   depPlatform,
				ArrPlatform:   arrPlatform,
				Cycle:         cycle,
				LineColor:     lineColor,
				TripID:        al.TripId,
			}

			legs = append(legs, leg)
			prevArrival = arr
		}

		if len(legs) == 0 {
			continue
		}

		// Apply filters
		if len(filters) > 0 {
			skip := false
			for _, leg := range legs {
				if enabled, exists := filters[leg.Product]; exists && !enabled {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
		}

		journeyStart, err := parseTime(aj.Legs[0].Departure)
		if err != nil {
			continue
		}
		lastArr := legs[len(legs)-1].Arrival
		if journeyStart.IsZero() || lastArr.IsZero() {
			continue
		}

		journey := Journey{
			LeaveAt:   journeyStart,
			ArriveAt:  lastArr,
			Duration:  lastArr.Sub(journeyStart),
			TotalWait: totalWait,
			Legs:      legs,
			IsNew:     true,
		}
		journeys = append(journeys, journey)
	}

	sort.Slice(journeys, func(i, j int) bool {
		if journeys[i].LeaveAt.Equal(journeys[j].LeaveAt) {
			return journeys[i].TotalWait < journeys[j].TotalWait
		}
		return journeys[i].LeaveAt.Before(journeys[j].LeaveAt)
	})

	return journeys, nil
}

// App holds the application state
type App struct {
	app         *tview.Application
	pages       *tview.Pages
	list        *tview.TextView
	detail      *tview.TextView
	header      *tview.TextView
	legend      *tview.TextView
	searchInput *tview.InputField
	searchList  *tview.List
	favList     *tview.List

	config         Config
	journeys       []Journey
	prevJourneyIDs map[string]bool
	selectedIdx    int
	lastUpdate     time.Time
	isLoading      bool

	filters map[string]bool

	searchTarget  string
	searchResults []Station

	// Animation state
	animFrame      int
	routeAnimFrame int
	refreshPulse   bool
	newHighlight   int // frames remaining for new highlight
	delayHistory   map[string]*DelayHistory
	delayHistoryMu sync.RWMutex

	// Status message
	statusMsg      string
	statusMsgFrame int

	// Splash screen
	showSplash  bool
	splashFrame int

	stopChan chan struct{}
}

// Berlin Bear ASCII Art
const berlinBearLogo = `
    ┌──────────────────────────────────────────────────────────────────┐
    │                                                                  │
    │                                    ↑↑↑↑↑                         │
    │                             ↑↑↑↑↑↑↑↑↑↑↑↑↑↑  ↑↑↑                  │
    │                           ↑↑↑↑↑↑  ↑ ↑↑↑↑↑↑↑↑↑↑                   │
    │                            ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                   │
    │       ↙↙↙↙↑             ↙↙      ↑↑↑↑↑↑↑↑↑↑↑↑↑↑                   │
    │      ↙↑↑↑↑↑↑                ↙↙↙↙↙ ↑↑↑↑↑↑↑↑↑↑↑↑                   │
    │       ↗↑↑↑↑↑                → ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                   │
    │        ↑↑↑↑↑↑                    ↑↑↑↑↑↑↑↑↑↑↑↑↑                   │
    │         ↑↑↑↑↑↑↑↑               ↑↑↑↑↑↑↑↑↑↑↑↑↑↑                    │
    │          ↑↑↑↑↑↑↑↑↑↑↑↑↑      ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                    │
    │           ↑↑↑↑↑↑↑↑↑↑↑↑↑↑  ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                    │
    │              ↑↑↑↑↑↑↑↑↑↑ ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                   │
    │                ↑↑↑↑↑↑↑↑ ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                    │
    │        ↙          ↑↑↑↑ ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                    │
    │      ↙↙↙↙                ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                     │
    │    ↙↙↙↑↑↑↑↑          ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                      │
    │     ↙↑↑↑↑↑↑↑     ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                      │
    │      ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                      │
    │        ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                     │
    │          ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑ ↘↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                     │
    │               ↑↑↑↑        ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                    │
    │                           ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                    │
    │                            ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                    │
    │                              ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                   │
    │                          ↑↑↑↑↑↗↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                   │
    │                       ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                  │
    │                     ↑↑↑↑↑↑↑↑↑↑↑ ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                 │
    │                    ↑↑↑↑↑↑↑↑↑↑↑↑ ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑                  │
    │                   ↑↑↑↑↑↑↑↑↑↑↑↑↑↑ ↑↑↑↑↑↑↑↑↑↑↑↑↑↑                  │
    │                  ↑↑↑↑↑↑↑↑↑↑↑↑↑↑↑ ↑↑↑↑↑↑↑↑↑↑↑↑↑↑                  │
    │                  ↑↑↑↑↑↑↑↑↑↑↑↑↑    ↑↑↑↑↑↑↑↑↑↑↑↑↑                  │
    │                  ↑↑↑↑↑↑↑↑↑↑↑       ↑↑↑↑↑↑↑↑↑↑↑↑                  │
    │           ↙      ↑↑↑↑↑↑↑↑↑          ↑↑↑↑↑↑↑↑↑↑↑↑                 │
    │          ↙↙↑↑↑↑   ↑↑↑↑↑↑↑             ↑↑↑↑↑↑↑↑↑↑                 │
    │         ↙↙↙↑↑↑↑↑↑↑↑↑↑↑↑↑                ↑↑↑↑↑↑↑↑↑                │
    │           ↙↑↑↑↑↑↑↑↑↑↑↑↑↑                ↑↑↑↑↑↑↑↑                 │
    │                ↑↑↑↑↑↑↑↑             ↙↑↑↑↑↑↑↑↑↑                   │
    │                                    ↙↙↑↑↑↑↑↑↑                     │
    │                                      ↙↑↑↑                        │
    │                                                                  │
        -------------------[-][-][yellow]BERRRRLIN ROUTER[-][-][-]-------------------     
    │                                                                  │
    └──────────────────────────────────────────────────────────────────┘
`

func NewApp() *App {
	a := &App{
		app:            tview.NewApplication(),
		pages:          tview.NewPages(),
		config:         loadConfig(),
		filters:        make(map[string]bool),
		prevJourneyIDs: make(map[string]bool),
		delayHistory:   make(map[string]*DelayHistory),
		stopChan:       make(chan struct{}),
		showSplash:     true,
		splashFrame:    20, // 2 seconds at 10fps
	}

	for _, p := range []string{"suburban", "subway", "tram", "bus", "ferry", "regional", "express"} {
		a.filters[p] = true
	}

	a.setupUI()
	return a
}

func (a *App) setupUI() {
	// Header with clock
	a.header = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)

	// Main list view
	a.list = tview.NewTextView().
		SetDynamicColors(true).
		SetRegions(true).
		SetScrollable(true)

	// Detail view
	a.detail = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true)
	a.detail.SetBorder(true).SetTitle(" Journey Details ")

	// Search components
	a.searchInput = tview.NewInputField().
		SetLabel("Search: ").
		SetFieldWidth(30)

	a.searchList = tview.NewList().
		SetHighlightFullLine(true).
		SetSelectedBackgroundColor(tcell.ColorBlue)

	searchFlex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(a.searchInput, 1, 0, true).
		AddItem(a.searchList, 0, 1, false)
	searchFlex.SetBorder(true).SetTitle(" Search Station ")

	// Favorites list
	a.favList = tview.NewList().
		SetHighlightFullLine(true).
		SetSelectedBackgroundColor(tcell.ColorBlue)
	a.favList.SetBorder(true).SetTitle(" Favorites (Enter=Load, a=Add current, d=Delete, Esc=Back) ")

	// Legend bar at bottom
	a.legend = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)
	a.legend.SetText("[dim]─────────────────────────────────────────────────────────────────────────[-]\n" +
		"[dim] Keys:[-] j/k Nav   Enter Detail   s Search   F Favorites   a Add Fav   R Reverse   r Refresh   q Quit\n" +
		"[dim] Legend:[-] [green]○ Low [yellow]◐ Med [red]● High Occupancy   [yellow]⏱ Delayed   [red]⚡ Tight Connection   [red]⚠ Warning   [green]★ New")

	// Splash screen
	splash := tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)
	splash.SetText(berlinBearLogo)

	// Main layout with legend
	mainFlex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(a.header, 3, 0, false).
		AddItem(a.list, 0, 1, true).
		AddItem(a.legend, 3, 0, false)

	a.pages.AddPage("splash", splash, true, true)
	a.pages.AddPage("main", mainFlex, true, false)
	a.pages.AddPage("detail", a.detail, true, false)
	a.pages.AddPage("search", searchFlex, true, false)
	a.pages.AddPage("favorites", a.favList, true, false)

	a.setupKeyBindings()
}

func (a *App) setupKeyBindings() {
	a.list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyUp:
			if a.selectedIdx > 0 {
				a.selectedIdx--
				a.routeAnimFrame = 0
			}
			return nil
		case tcell.KeyDown:
			if a.selectedIdx < len(a.journeys)-1 {
				a.selectedIdx++
				a.routeAnimFrame = 0
			}
			return nil
		case tcell.KeyEnter:
			if len(a.journeys) > 0 {
				a.showDetail()
			}
			return nil
		case tcell.KeyRune:
			switch event.Rune() {
			case 'k':
				if a.selectedIdx > 0 {
					a.selectedIdx--
					a.routeAnimFrame = 0
				}
				return nil
			case 'j':
				if a.selectedIdx < len(a.journeys)-1 {
					a.selectedIdx++
					a.routeAnimFrame = 0
				}
				return nil
			case 'r':
				a.refresh()
				return nil
			case 'R':
				a.config.LastOrigin, a.config.LastDest = a.config.LastDest, a.config.LastOrigin
				saveConfig(a.config)
				a.refresh()
				return nil
			case 's':
				a.showSearch("origin")
				return nil
			case 'F':
				a.showFavorites()
				return nil
			case 'a':
				a.addFavorite()
				return nil
			case 'q':
				close(a.stopChan)
				a.app.Stop()
				return nil
			}
		}
		return event
	})

	a.detail.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEscape:
			a.pages.SwitchToPage("main")
			a.app.SetFocus(a.list)
			return nil
		case tcell.KeyRune:
			if event.Rune() == 'q' || event.Rune() == 'b' {
				a.pages.SwitchToPage("main")
				a.app.SetFocus(a.list)
				return nil
			}
		}
		return event
	})

	a.searchInput.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEscape {
			a.pages.SwitchToPage("main")
			a.app.SetFocus(a.list)
		} else if key == tcell.KeyEnter || key == tcell.KeyTab {
			if a.searchList.GetItemCount() > 0 {
				a.app.SetFocus(a.searchList)
			}
		}
	})

	a.searchInput.SetChangedFunc(func(text string) {
		if len(text) >= 2 {
			go func() {
				stations, err := searchStations(text)
				if err != nil {
					return
				}
				a.searchResults = stations
				a.app.QueueUpdateDraw(func() {
					a.searchList.Clear()
					for _, s := range stations {
						station := s
						a.searchList.AddItem(s.Name, "", 0, func() {
							a.selectStation(station)
						})
					}
				})
			}()
		}
	})

	a.searchList.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.pages.SwitchToPage("main")
			a.app.SetFocus(a.list)
			return nil
		}
		return event
	})
}

func (a *App) selectStation(station Station) {
	if a.searchTarget == "origin" {
		a.config.LastOrigin = station
		a.searchTarget = "dest"
		a.searchInput.SetText("")
		a.searchInput.SetLabel("Destination: ")
		a.searchList.Clear()
		a.app.SetFocus(a.searchInput)
	} else {
		a.config.LastDest = station
		saveConfig(a.config)
		a.pages.SwitchToPage("main")
		a.app.SetFocus(a.list)
		a.refresh()
	}
}

func (a *App) showSearch(target string) {
	a.searchTarget = target
	a.searchInput.SetText("")
	if target == "origin" {
		a.searchInput.SetLabel("Origin: ")
	} else {
		a.searchInput.SetLabel("Destination: ")
	}
	a.searchList.Clear()
	a.pages.SwitchToPage("search")
	a.app.SetFocus(a.searchInput)
}

func (a *App) showFavorites() {
	a.favList.Clear()

	if len(a.config.Routes) == 0 {
		a.favList.AddItem("No favorites saved", "Press 'a' on main screen to add current route", 0, nil)
	} else {
		for i, fav := range a.config.Routes {
			idx := i
			origin := cleanStation(fav.Origin.Name)
			dest := cleanStation(fav.Dest.Name)
			a.favList.AddItem(fmt.Sprintf("%s → %s", origin, dest), "", 0, func() {
				a.loadFavorite(idx)
			})
		}
	}

	a.favList.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEscape:
			a.pages.SwitchToPage("main")
			a.app.SetFocus(a.list)
			return nil
		case tcell.KeyRune:
			if event.Rune() == 'd' && len(a.config.Routes) > 0 {
				idx := a.favList.GetCurrentItem()
				if idx >= 0 && idx < len(a.config.Routes) {
					a.config.Routes = append(a.config.Routes[:idx], a.config.Routes[idx+1:]...)
					saveConfig(a.config)
					a.showFavorites()
				}
				return nil
			}
		}
		return event
	})

	a.pages.SwitchToPage("favorites")
	a.app.SetFocus(a.favList)
}

func (a *App) loadFavorite(idx int) {
	if idx >= 0 && idx < len(a.config.Routes) {
		fav := a.config.Routes[idx]
		a.config.LastOrigin = fav.Origin
		a.config.LastDest = fav.Dest
		saveConfig(a.config)
		a.pages.SwitchToPage("main")
		a.app.SetFocus(a.list)
		a.refresh()
	}
}

func (a *App) addFavorite() {
	// Check if already exists
	for _, fav := range a.config.Routes {
		if fav.Origin.ID == a.config.LastOrigin.ID && fav.Dest.ID == a.config.LastDest.ID {
			a.statusMsg = "Already in favorites"
			a.statusMsgFrame = 30
			return
		}
	}

	a.config.Routes = append(a.config.Routes, FavoriteRoute{
		Origin: a.config.LastOrigin,
		Dest:   a.config.LastDest,
	})
	saveConfig(a.config)
	a.statusMsg = "★ Added to favorites!"
	a.statusMsgFrame = 30
}

func (a *App) showDetail() {
	if a.selectedIdx >= len(a.journeys) {
		return
	}

	j := a.journeys[a.selectedIdx]
	var sb strings.Builder

	countdown := time.Until(j.LeaveAt)
	countdownStr := formatCountdown(countdown)

	sb.WriteString(fmt.Sprintf("[yellow::b]Journey: %s → %s[-:-:-]  Departs in: %s\n",
		formatTime(j.LeaveAt), formatTime(j.ArriveAt), countdownStr))
	sb.WriteString(fmt.Sprintf("Duration: %dmin  |  Total wait: %dmin\n",
		int(j.Duration.Minutes()), int(j.TotalWait.Minutes())))
	sb.WriteString(strings.Repeat("─", 55) + "\n\n")

	now := time.Now()

	for i, leg := range j.Legs {
		// Wait time with tight connection warning
		if leg.WaitBefore > 0 {
			waitMins := int(leg.WaitBefore.Minutes())
			if waitMins <= 2 {
				sb.WriteString(fmt.Sprintf("[red::b]  ⚡ TIGHT CONNECTION: %dmin to change![-:-:-]\n", waitMins))
			} else {
				sb.WriteString(fmt.Sprintf("[yellow]  ⏱ Wait %dmin[-]\n", waitMins))
			}
		}

		color := getProductColor(leg.Product)

		// Delay with pulse effect
		delayStr := ""
		if leg.DepDelay > 0 {
			delayStr = fmt.Sprintf(" [red::b]+%dm[-:-:-]", leg.DepDelay/60)
		}

		// Animated occupancy bar
		occBar := occupancyBar(leg.Occupancy, a.animFrame)

		cycleStr := ""
		if leg.Cycle > 0 {
			cycleStr = fmt.Sprintf(" (every %dm)", leg.Cycle)
		}

		// Delay sparkline history
		sparkStr := ""
		a.delayHistoryMu.RLock()
		if hist, ok := a.delayHistory[leg.Line]; ok && len(hist.Delays) > 0 {
			sparkStr = fmt.Sprintf(" [dim]%s[-]", sparkline(hist.Delays, 8))
		}
		a.delayHistoryMu.RUnlock()

		sb.WriteString(fmt.Sprintf("[%s::b]%s %s[-:-:-] %s → %s%s  %s%s%s\n",
			color, getProductIcon(leg.Product), leg.Line,
			formatTime(leg.Departure), formatTime(leg.Arrival),
			delayStr, occBar, cycleStr, sparkStr))

		// Vehicle position tracker - show if journey is in progress
		if now.After(leg.Departure) && now.Before(leg.Arrival) {
			elapsed := now.Sub(leg.Departure)
			total := leg.Arrival.Sub(leg.Departure)
			progress := float64(elapsed) / float64(total)
			pos := int(progress * 20)
			if pos > 19 {
				pos = 19
			}
			bar := strings.Repeat("─", pos) + "●" + strings.Repeat("─", 19-pos)
			sb.WriteString(fmt.Sprintf("    [%s]%s[-] [dim]in transit[-]\n", color, bar))
		}

		// Stations with platforms
		fromPlt := ""
		if leg.DepPlatform != "" {
			fromPlt = fmt.Sprintf(" [cyan][Plt %s][-]", leg.DepPlatform)
		}
		toPlt := ""
		if leg.ArrPlatform != "" {
			toPlt = fmt.Sprintf(" [cyan][Plt %s][-]", leg.ArrPlatform)
		}

		sb.WriteString(fmt.Sprintf("    From: %s%s\n", cleanStation(leg.From), fromPlt))
		sb.WriteString(fmt.Sprintf("    To:   %s%s\n", cleanStation(leg.To), toPlt))

		// Service warnings
		for _, status := range leg.ServiceStatus {
			if len(status) > 50 {
				status = status[:50] + "..."
			}
			sb.WriteString(fmt.Sprintf("    [red]⚠ %s[-]\n", status))
		}

		if i < len(j.Legs)-1 {
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\n\n[dim]Press ESC or 'b' to go back[-]")

	a.detail.SetText(sb.String())
	a.pages.SwitchToPage("detail")
	a.app.SetFocus(a.detail)
}

func (a *App) renderHeader() {
	now := time.Now()
	clock := now.Format("15:04:05")

	origin := cleanStation(a.config.LastOrigin.Name)
	dest := cleanStation(a.config.LastDest.Name)
	if len(origin) > 15 {
		origin = origin[:15]
	}
	if len(dest) > 15 {
		dest = dest[:15]
	}

	spinner := ""
	if a.isLoading {
		spinner = fmt.Sprintf(" %s", spinnerFrames[a.animFrame%len(spinnerFrames)])
	}

	// Status message display
	statusDisplay := ""
	if a.statusMsgFrame > 0 {
		statusDisplay = fmt.Sprintf("  [green::b]%s[-:-:-]", a.statusMsg)
	}

	// Pulse effect on refresh
	borderColor := "yellow"
	if a.refreshPulse && a.animFrame%4 < 2 {
		borderColor = "green"
	}

	header := fmt.Sprintf("[%s]╔═════════════════════════════════════════════════════════════════════╗[-]\n", borderColor)
	header += fmt.Sprintf("[%s]   [-] [::b]BERRRRLIN ROUTER [-:-:-]  %s → %s  [cyan]%s[-]%s%s  [%s]  [-]\n",
		borderColor, origin, dest, clock, spinner, statusDisplay, borderColor)
	header += fmt.Sprintf("[%s]╚═════════════════════════════════════════════════════════════════════╝[-]", borderColor)

	a.header.SetText(header)
}

func (a *App) renderList() {
	var sb strings.Builder

	if len(a.journeys) == 0 {
		if a.isLoading {
			spinner := spinnerFrames[a.animFrame%len(spinnerFrames)]
			sb.WriteString(fmt.Sprintf("\n  %s [dim]Loading routes...[-]\n", spinner))
		} else {
			sb.WriteString("\n [dim]No journeys found. Press 'r' to refresh.[-]\n")
		}
		a.list.SetText(sb.String())
		return
	}

	now := time.Now()

	for i, j := range a.journeys {
		waitMins := int(j.TotalWait.Minutes())
		durMins := int(j.Duration.Minutes())
		countdown := j.LeaveAt.Sub(now)

		// Check statuses
		hasDelay := false
		hasWarning := false
		hasTightConnection := false
		maxOcc := ""
		occPriority := map[string]int{"low": 1, "medium": 2, "high": 3}

		for _, leg := range j.Legs {
			if leg.DepDelay > 0 {
				hasDelay = true
			}
			if len(leg.ServiceStatus) > 0 {
				hasWarning = true
			}
			if leg.WaitBefore > 0 && leg.WaitBefore.Minutes() <= 2 {
				hasTightConnection = true
			}
			if leg.Occupancy != "" {
				if maxOcc == "" || occPriority[leg.Occupancy] > occPriority[maxOcc] {
					maxOcc = leg.Occupancy
				}
			}
		}

		isSelected := i == a.selectedIdx
		selector := "  "
		headerStyle := ""

		if isSelected {
			selector = "[::r] ▸ [-:-:-]"
			headerStyle = "::b"
		}

		// Color based on status (static)
		headerColor := "white"
		if hasDelay {
			headerColor = "yellow"
		} else if countdown < 5*time.Minute && countdown > 0 {
			headerColor = "red"
		} else if waitMins <= 5 {
			headerColor = "green"
		} else if waitMins <= 10 {
			headerColor = "yellow"
		}

		// New journey indicator (static)
		newIndicator := ""
		if j.IsNew && a.newHighlight > 0 {
			newIndicator = " [green]★[-]"
		}

		// Tight connection indicator (static)
		tightStr := ""
		if hasTightConnection {
			tightStr = " [red]⚡[-]"
		}

		// Occupancy indicator (static)
		occStr := ""
		switch maxOcc {
		case "low":
			occStr = " [green]○[-]"
		case "medium":
			occStr = " [yellow]◐[-]"
		case "high":
			occStr = " [red]●[-]"
		}

		warnStr := ""
		if hasWarning {
			warnStr = " [red]⚠[-]"
		}

		delayStr := ""
		if hasDelay {
			delayStr = " [yellow]⏱[-]"
		}

		countdownStr := formatCountdown(countdown)

		// Header line with countdown
		sb.WriteString(fmt.Sprintf("%s[%s%s]%d. %s → %s  (%dm)  wait:%dm[-:-:-]  %s%s%s%s%s%s\n",
			selector, headerColor, headerStyle, i+1,
			formatTime(j.LeaveAt), formatTime(j.ArriveAt),
			durMins, waitMins, countdownStr, occStr, delayStr, tightStr, warnStr, newIndicator))

		// Visual route with colored circles (static)
		sb.WriteString("    ")
		for li, leg := range j.Legs {
			color := getProductColor(leg.Product)
			circle := fmt.Sprintf("[%s]●[-]", color)

			if li == 0 {
				sb.WriteString(circle)
			}

			sb.WriteString(fmt.Sprintf("[%s]─%s─[-]", color, leg.Line))
			sb.WriteString(circle)
		}
		sb.WriteString("\n")

		// Separator
		sb.WriteString("    [dim]" + strings.Repeat("─", 50) + "[-]\n")
	}

	a.list.SetText(sb.String())
}

func (a *App) refresh() {
	a.isLoading = true
	a.refreshPulse = true

	go func() {
		journeys, err := fetchJourneys(a.config.LastOrigin.ID, a.config.LastDest.ID, nil)

		a.app.QueueUpdateDraw(func() {
			if err != nil {
				a.journeys = nil
			} else {
				// Detect new journeys
				newIDs := make(map[string]bool)
				hasNew := false
				for i := range journeys {
					id := fmt.Sprintf("%s-%s", journeys[i].LeaveAt.Format(time.RFC3339), journeys[i].Legs[0].Line)
					newIDs[id] = true
					if !a.prevJourneyIDs[id] {
						journeys[i].IsNew = true
						hasNew = true
					} else {
						journeys[i].IsNew = false
					}
				}
				a.prevJourneyIDs = newIDs

				if hasNew {
					a.newHighlight = 30 // Flash for 30 frames (~3 seconds)
				}

				// Update delay history for sparklines
				a.delayHistoryMu.Lock()
				for _, j := range journeys {
					for _, leg := range j.Legs {
						if leg.DepDelay > 0 {
							if _, ok := a.delayHistory[leg.Line]; !ok {
								a.delayHistory[leg.Line] = &DelayHistory{Line: leg.Line}
							}
							hist := a.delayHistory[leg.Line]
							hist.Delays = append(hist.Delays, leg.DepDelay/60)
							if len(hist.Delays) > 20 {
								hist.Delays = hist.Delays[len(hist.Delays)-20:]
							}
							hist.Updated = time.Now()
						}
					}
				}
				a.delayHistoryMu.Unlock()

				a.journeys = journeys
			}
			a.lastUpdate = time.Now()
			a.selectedIdx = 0
			a.isLoading = false

			// Stop refresh pulse after a moment
			go func() {
				time.Sleep(500 * time.Millisecond)
				a.refreshPulse = false
			}()
		})
	}()
}

func (a *App) startAnimationLoop() {
	ticker := time.NewTicker(100 * time.Millisecond) // 10 FPS
	refreshTicker := time.NewTicker(30 * time.Second)

	go func() {
		for {
			select {
			case <-a.stopChan:
				ticker.Stop()
				refreshTicker.Stop()
				return
			case <-ticker.C:
				a.animFrame++
				if a.selectedIdx < len(a.journeys) {
					a.routeAnimFrame++
				}

				// Splash screen countdown
				if a.showSplash {
					a.splashFrame--
					if a.splashFrame <= 0 {
						a.showSplash = false
						a.app.QueueUpdateDraw(func() {
							a.pages.SwitchToPage("main")
							a.app.SetFocus(a.list)
							a.refresh()
						})
					}
					continue
				}

				// Decrement new highlight counter
				if a.newHighlight > 0 {
					a.newHighlight--
				}

				// Decrement status message counter
				if a.statusMsgFrame > 0 {
					a.statusMsgFrame--
				}

				// Clear IsNew after animation
				if a.animFrame > 50 {
					for i := range a.journeys {
						a.journeys[i].IsNew = false
					}
				}

				a.app.QueueUpdateDraw(func() {
					a.renderHeader()
					a.renderList()
				})
			case <-refreshTicker.C:
				a.refresh()
			}
		}
	}()
}

func (a *App) Run() error {
	a.isLoading = true // Show loading spinner after splash
	a.startAnimationLoop()
	return a.app.SetRoot(a.pages, true).EnableMouse(true).Run()
}

func main() {
	app := NewApp()
	if err := app.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
