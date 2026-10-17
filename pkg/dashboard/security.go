package dashboard

import (
	"encoding/json"
	"strings"
)

// securityDoc is the security artifact the enricher writes: one JSON object.
type securityDoc struct {
	Facts       map[string]bool   `json:"facts"`
	Notes       map[string]string `json:"notes"`
	Permissions []string          `json:"permissions"`
	Notice      string            `json:"notice"`
	Body        string            `json:"body"`
	Keywords    []string          `json:"keywords"`
}

type securityView struct {
	Facts      []factView
	Unreported []string
	Sensitive  []permissionView
	Other      []permissionView
	Notice     string
	Body       string
	Keywords   []string
	// Raw is set when the artifact is not valid JSON, so it is still visible.
	Raw string
}

// factView is one claim. Tone is "ok" for evidence in the user's favor,
// "bad" for a finding against, and "" for neutral.
type factView struct {
	Icon  string
	Label string
	Note  string
	Tone  string
}

type permissionView struct {
	ID    string
	Icon  string
	Label string
	Note  string
}

// fact names in the order the enricher and the app page share.
var factOrder = []string{"google_services", "tracking", "ads", "offline_capable", "e2ee", "source_available"}

type factText struct {
	icon            string
	yes, no         string
	yesTone, noTone string
	unreported      string
}

var facts = map[string]factText{
	"google_services":  {icon: "cloud", yes: "Uses Google services", no: "No Google services", yesTone: "bad", noTone: "ok", unreported: "Google services"},
	"tracking":         {icon: "eye", yes: "Tracks users", yesTone: "bad", unreported: "Tracking"},
	"ads":              {icon: "megaphone", yes: "Shows ads", yesTone: "bad", unreported: "Ads"},
	"offline_capable":  {icon: "wifi-off", yes: "Works offline", no: "Needs a connection", yesTone: "ok", unreported: "Offline use"},
	"e2ee":             {icon: "lock", yes: "End-to-end encrypted", yesTone: "ok", unreported: "Encryption"},
	"source_available": {icon: "code", yes: "Source available", no: "No source", yesTone: "ok", noTone: "bad", unreported: "Source availability"},
}

// parseSecurity reads the artifact. An empty artifact is nil.
func parseSecurity(raw string) *securityView {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var doc securityDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return &securityView{Raw: raw}
	}
	v := &securityView{
		Notice:   strings.TrimSpace(doc.Notice),
		Body:     strings.TrimSpace(doc.Body),
		Keywords: doc.Keywords,
	}
	for _, name := range factOrder {
		text := facts[name]
		value, ok := doc.Facts[name]
		if !ok {
			v.Unreported = append(v.Unreported, text.unreported)
			continue
		}
		f := factView{Icon: text.icon, Label: text.yes, Tone: text.yesTone, Note: doc.Notes[name]}
		if !value {
			f.Label, f.Tone = text.no, text.noTone
		}
		if f.Label == "" {
			v.Unreported = append(v.Unreported, text.unreported)
			continue
		}
		v.Facts = append(v.Facts, f)
	}
	for _, id := range doc.Permissions {
		p, sensitive := describePermission(id)
		p.Note = doc.Notes[id]
		if sensitive {
			v.Sensitive = append(v.Sensitive, p)
		} else {
			v.Other = append(v.Other, p)
		}
	}
	return v
}

type permissionText struct {
	icon      string
	label     string
	sensitive bool
}

var permissions = map[string]permissionText{
	"CAMERA":                     {"camera", "Camera", true},
	"RECORD_AUDIO":               {"mic", "Microphone", true},
	"ACCESS_FINE_LOCATION":       {"map-pin", "Precise location", true},
	"ACCESS_COARSE_LOCATION":     {"map-pin", "Approximate location", true},
	"READ_CONTACTS":              {"users", "Read contacts", true},
	"WRITE_CONTACTS":             {"users", "Change contacts", true},
	"GET_ACCOUNTS":               {"users", "Find accounts on the device", true},
	"READ_SMS":                   {"message", "Read SMS", true},
	"SEND_SMS":                   {"message", "Send SMS", true},
	"RECEIVE_SMS":                {"message", "Receive SMS", true},
	"READ_CALL_LOG":              {"phone", "Read call log", true},
	"CALL_PHONE":                 {"phone", "Make phone calls", true},
	"READ_PHONE_STATE":           {"phone", "Read phone status", true},
	"READ_PHONE_NUMBERS":         {"phone", "Read phone numbers", true},
	"READ_CALENDAR":              {"calendar", "Read calendar", true},
	"WRITE_CALENDAR":             {"calendar", "Change calendar", true},
	"BODY_SENSORS":               {"activity", "Body sensors", true},
	"ACTIVITY_RECOGNITION":       {"activity", "Physical activity", true},
	"MANAGE_EXTERNAL_STORAGE":    {"folder", "All files", true},
	"SYSTEM_ALERT_WINDOW":        {"layers", "Draw over other apps", true},
	"REQUEST_INSTALL_PACKAGES":   {"package", "Install other apps", true},
	"QUERY_ALL_PACKAGES":         {"grid", "See all installed apps", true},
	"BIND_ACCESSIBILITY_SERVICE": {"eye", "Accessibility service", true},
	"BIND_DEVICE_ADMIN":          {"shield", "Device admin", true},
	"READ_EXTERNAL_STORAGE":      {"folder", "Read shared storage", false},
	"WRITE_EXTERNAL_STORAGE":     {"folder", "Write shared storage", false},
	"READ_MEDIA_IMAGES":          {"image", "Photos", false},
	"READ_MEDIA_VIDEO":           {"image", "Videos", false},
	"READ_MEDIA_AUDIO":           {"mic", "Audio files", false},
	"INTERNET":                   {"globe", "Internet", false},
	"ACCESS_NETWORK_STATE":       {"wifi", "Network status", false},
	"ACCESS_WIFI_STATE":          {"wifi", "Wi-Fi status", false},
	"CHANGE_WIFI_STATE":          {"wifi", "Change Wi-Fi", false},
	"BLUETOOTH":                  {"bluetooth", "Bluetooth", false},
	"BLUETOOTH_CONNECT":          {"bluetooth", "Connect Bluetooth devices", false},
	"BLUETOOTH_SCAN":             {"bluetooth", "Scan for Bluetooth devices", false},
	"NFC":                        {"smartphone", "NFC", false},
	"POST_NOTIFICATIONS":         {"bell", "Notifications", false},
	"VIBRATE":                    {"smartphone", "Vibrate", false},
	"WAKE_LOCK":                  {"clock", "Keep the device awake", false},
	"RECEIVE_BOOT_COMPLETED":     {"clock", "Start at boot", false},
	"FOREGROUND_SERVICE":         {"activity", "Run in the foreground", false},
	"USE_BIOMETRIC":              {"key", "Biometrics", false},
	"USE_FINGERPRINT":            {"key", "Fingerprint", false},
}

// describePermission labels an Android permission id, with or without the
// android.permission. prefix. Unknown ids get a label from their name.
func describePermission(id string) (permissionView, bool) {
	name := id
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if text, ok := permissions[name]; ok {
		return permissionView{ID: id, Icon: text.icon, Label: text.label}, text.sensitive
	}
	words := strings.Fields(strings.ToLower(strings.ReplaceAll(name, "_", " ")))
	if len(words) > 0 {
		words[0] = strings.ToUpper(words[0][:1]) + words[0][1:]
	}
	label := strings.Join(words, " ")
	if label == "" {
		label = id
	}
	return permissionView{ID: id, Icon: "key", Label: label}, false
}
