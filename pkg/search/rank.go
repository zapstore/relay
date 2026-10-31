// Package search ranks catalog apps as product/specs/search.md defines.
// The constants and rules match the Android ranker, and the fact phrases match
// webapp/modules/apps/search-facts.ts; tests/fixtures/search.json checks them.
package search

import (
	"encoding/json"
	"math/bits"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// An int8 dot cannot pass an exact name or app id.
const (
	VectorDims      = 768
	MaxQueryBytes   = 256
	MinVectorDot    = 57_348
	PrefixBoost     = 80_645
	WordBoost       = 71_684
	EditBoost       = 35_842
	ExactBoost      = 20_000_000
	FactScore       = 18_000
	HardFactScore   = 54_000
	MinSearchLength = 2
	Limit           = 20
)

// Fact bits. 1<<3 was accountless, 1<<4 tracking, 1<<5 decentralized,
// 1<<6 ads, 1<<7 e2ee. Do not reuse them.
const (
	FactSourceAvailable = 1
	FactGoogle          = 1 << 1
	FactOffline         = 1 << 2
)

// Facts are the search facts, by name.
var Facts = []string{"source_available", "google_services", "offline_capable"}

// HardBit is the bit a fact must have set. No Google services sets none; it excludes instead.
func HardBit(fact string) int {
	switch fact {
	case "source_available":
		return FactSourceAvailable
	case "offline_capable":
		return FactOffline
	}
	return 0
}

// ExcludeBit is the bit a fact must have clear.
func ExcludeBit(fact string) int {
	if fact == "google_services" {
		return FactGoogle
	}
	return 0
}

// Parsed is a query with its fact phrases taken out. Hard bits must be set
// and Exclude bits clear. Residual is the text for exact matching; the
// encoder sees the whole normalized query.
type Parsed struct {
	Residual string
	Boost    int
	Penalty  int
	Hard     int
	Exclude  int
	Facts    []string
}

// App is one ranked listing.
type App struct {
	ID       string
	Name     string
	Vector   []int8
	FactBits int
	Keywords []string
}

type pill struct {
	fact       string
	bit        int
	words      []string
	keywords   []string
	phraseOnly bool
	penalty    bool
}

// Same phrases as Android Facts.kt. "offline" stands alone; the rest match only as a phrase.
var pills = []pill{
	{fact: "google_services", bit: FactGoogle, words: []string{"no", "google", "services"}, phraseOnly: true, penalty: true},
	{fact: "offline_capable", bit: FactOffline, words: []string{"works", "offline"}, keywords: []string{"offline"}},
	{fact: "source_available", bit: FactSourceAvailable, words: []string{"source", "available"}, phraseOnly: true},
	{fact: "source_available", bit: FactSourceAvailable, words: []string{"open", "source"}, phraseOnly: true},
}

var pillByKey = map[string]int{
	"google_services":  FactGoogle,
	"offline_capable":  FactOffline,
	"source_available": FactSourceAvailable,
	"open_source":      FactSourceAvailable,
}

// Normalize trims, collapses whitespace, lowercases, and cuts to MaxQueryBytes.
func Normalize(query string) string {
	folded := Fold(query)
	if len(folded) <= MaxQueryBytes {
		return folded
	}
	end := MaxQueryBytes
	for end > 0 && !utf8.RuneStart(folded[end]) {
		end--
	}
	return strings.TrimRightFunc(folded[:end], unicode.IsSpace)
}

// Parse pulls fact phrases out of a query. A matched phrase is removed from
// the residual and becomes a hard filter. "No Google services" excludes apps
// with Google services.
func Parse(raw string) Parsed {
	folded := Fold(strings.ReplaceAll(strings.ReplaceAll(raw, "'", ""), "-", " "))
	if folded == "" {
		return Parsed{}
	}
	tokens := strings.Split(folded, " ")
	consumed := make([]bool, len(tokens))
	var p Parsed
	for index := 0; index < len(tokens); {
		h, ok := matchKeyword(tokens, index)
		if !ok {
			index++
			continue
		}
		if !slices.Contains(p.Facts, h.fact) {
			p.Facts = append(p.Facts, h.fact)
		}
		if h.penalty {
			p.Penalty |= h.bit
		} else {
			p.Boost |= h.bit
		}
		for offset := 0; offset < h.length; offset++ {
			consumed[h.start+offset] = true
		}
		index = h.start + h.length
	}
	var rest []string
	for at, token := range tokens {
		if !consumed[at] {
			rest = append(rest, token)
		}
	}
	p.Residual = strings.Join(rest, " ")
	p.Hard, p.Exclude = p.Boost, p.Penalty
	return p
}

// Rank orders apps for query, the residual text. Hard bits must all be set
// and exclude bits clear. A hard filter narrows the text's results and never
// widens them: with residual text, an app still needs a text hit or a vector
// over the cutoff. Only a query that is nothing but fact phrases (an empty
// residual) lets the filter choose the set.
func Rank(apps []App, query string, vector []int8, boost, limit, penalty int, admitAll bool, hard, exclude int) []App {
	if limit == 0 {
		return nil
	}
	if len(vector) != VectorDims {
		vector = nil
	}
	keepAll := admitAll || query == ""
	type scored struct {
		app   App
		score int
	}
	var kept []scored
	for _, app := range apps {
		if app.FactBits&hard != hard || app.FactBits&exclude != 0 {
			continue
		}
		score, ok := scoreApp(query, app, vector, boost, keepAll, penalty)
		if ok {
			kept = append(kept, scored{app, score})
		}
	}
	slices.SortStableFunc(kept, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return strings.Compare(a.app.ID, b.app.ID)
	})
	out := make([]App, 0, min(limit, len(kept)))
	for _, item := range kept[:min(limit, len(kept))] {
		out = append(out, item.app)
	}
	return out
}

func scoreApp(query string, app App, vector []int8, boost int, admitAll bool, penalty int) (int, bool) {
	text := textBoost(query, app.Name, app.ID) + keywordScore(query, app.Keywords)
	hasDot := vector != nil && len(app.Vector) == VectorDims
	dot := 0
	if hasDot {
		dot = vectorDot(vector, app.Vector)
	}
	if !admitAll && text == 0 && (!hasDot || dot < MinVectorDot) {
		return 0, false
	}
	return text + dot + factScore(app.FactBits, boost, penalty), true
}

func factScore(set, boost, penalty int) int {
	return bits.OnesCount(uint(set&boost))*HardFactScore - bits.OnesCount(uint(set&penalty))*FactScore
}

func textBoost(query, name, appID string) int {
	if query == "" {
		return 0
	}
	foldedName := Fold(name)
	foldedID := strings.ToLower(appID)
	if query == foldedName || query == foldedID {
		return ExactBoost
	}
	if strings.HasPrefix(foldedName, query) || strings.HasPrefix(foldedID, query) {
		return PrefixBoost
	}
	words := strings.Split(query, " ")
	nameWords := nonEmpty(strings.Split(foldedName, " "))
	idWords := nonEmpty(strings.FieldsFunc(foldedID, func(r rune) bool { return r == '.' || r == '-' || r == '_' }))
	if covers(words, nameWords) || covers(words, idWords) {
		return WordBoost
	}
	if coversWithOneEdit(words, nameWords) || coversWithOneEdit(words, idWords) {
		return EditBoost
	}
	return 0
}

// keywordScore is about cosine 0.30 per matched query word, at most two words.
func keywordScore(query string, keywords []string) int {
	if query == "" || len(keywords) == 0 {
		return 0
	}
	words := strings.Split(query, " ")
	used := make([]bool, len(words))
	matched := 0
	for _, keyword := range keywords {
		parts := nonEmpty(strings.Split(keyword, " "))
		if len(parts) == 0 {
			continue
		}
		for start := 0; start+len(parts) <= len(words); {
			hit := true
			for index, part := range parts {
				if used[start+index] || !keywordWord(words[start+index], part) {
					hit = false
					break
				}
			}
			if !hit {
				start++
				continue
			}
			for index := range parts {
				used[start+index] = true
			}
			matched += len(parts)
			start += len(parts)
		}
	}
	return min(matched, 2) * HardFactScore
}

func covers(queryWords, fieldWords []string) bool {
	if len(queryWords) == 0 || len(fieldWords) == 0 {
		return false
	}
	for _, word := range queryWords {
		if !slices.ContainsFunc(fieldWords, func(field string) bool { return wordEqualsOrPrefix(word, field) }) {
			return false
		}
	}
	return true
}

func coversWithOneEdit(queryWords, fieldWords []string) bool {
	if len(queryWords) == 0 || len(fieldWords) == 0 {
		return false
	}
	edits := 0
	for _, word := range queryWords {
		if slices.ContainsFunc(fieldWords, func(field string) bool { return wordEqualsOrPrefix(word, field) }) {
			continue
		}
		if edits == 1 || !slices.ContainsFunc(fieldWords, func(field string) bool { return oneEdit(word, field) }) {
			return false
		}
		edits++
	}
	return edits == 1
}

func wordEqualsOrPrefix(query, field string) bool {
	if query == field {
		return true
	}
	return runeLen(query) > 1 && strings.HasPrefix(field, query)
}

func oneEdit(query, field string) bool {
	return runeLen(query) >= 5 && runeLen(field) >= 5 && levenshteinAtMostOne(query, field)
}

func keywordWord(query, listed string) bool {
	if query == listed {
		return true
	}
	if runeLen(query) >= 5 && strings.HasSuffix(query, "s") && strings.TrimSuffix(query, "s") == listed {
		return true
	}
	return runeLen(listed) >= 5 && strings.HasSuffix(listed, "s") && strings.TrimSuffix(listed, "s") == query
}

func vectorDot(left, right []int8) int {
	sum := 0
	for i := 0; i < VectorDims; i++ {
		sum += int(left[i]) * int(right[i])
	}
	return sum
}

// Fold trims, collapses whitespace to one space, and lowercases each character.
func Fold(value string) string {
	var out strings.Builder
	pending := false
	for _, r := range strings.TrimFunc(value, unicode.IsSpace) {
		if unicode.IsSpace(r) {
			pending = out.Len() > 0
			continue
		}
		if pending {
			out.WriteByte(' ')
			pending = false
		}
		out.WriteRune(unicode.ToLower(r))
	}
	return out.String()
}

type hit struct {
	fact    string
	bit     int
	start   int
	length  int
	penalty bool
}

func matchKeyword(tokens []string, index int) (hit, bool) {
	var found hit
	ok := false
	for _, label := range pills {
		var h hit
		var matched bool
		if label.phraseOnly {
			h, matched = phraseHit(tokens, index, label)
		} else {
			h, matched = keywordHit(tokens, index, label)
		}
		if matched && (!ok || h.length > found.length) {
			found, ok = h, true
		}
	}
	return found, ok
}

func phraseHit(tokens []string, start int, label pill) (hit, bool) {
	if !fits(tokens, start, label.words) {
		return hit{}, false
	}
	return hit{label.fact, label.bit, start, len(label.words), label.penalty}, true
}

func keywordHit(tokens []string, index int, label pill) (hit, bool) {
	for _, keyword := range label.keywords {
		if tokenMatches(tokens[index], keyword) {
			return expand(tokens, index, label, keyword), true
		}
	}
	return hit{}, false
}

func fits(tokens []string, start int, phrase []string) bool {
	if start+len(phrase) > len(tokens) {
		return false
	}
	for offset, word := range phrase {
		if !tokenMatches(tokens[start+offset], word) {
			return false
		}
	}
	return true
}

func expand(tokens []string, index int, label pill, keyword string) hit {
	at := slices.IndexFunc(label.words, func(word string) bool { return tokenMatches(keyword, word) })
	if at < 0 {
		return hit{label.fact, label.bit, index, 1, label.penalty}
	}
	start, word := index, at
	for word > 0 && start > 0 && tokenMatches(tokens[start-1], label.words[word-1]) {
		start--
		word--
	}
	end, word := index, at
	for word+1 < len(label.words) && end+1 < len(tokens) && tokenMatches(tokens[end+1], label.words[word+1]) {
		end++
		word++
	}
	return hit{label.fact, label.bit, start, end - start + 1, label.penalty}
}

func tokenMatches(query, listed string) bool {
	if query == listed {
		return true
	}
	if stem, ok := pluralStem(query); ok && stem == listed {
		return true
	}
	if stem, ok := pluralStem(listed); ok && stem == query {
		return true
	}
	return runeLen(query) >= 6 && runeLen(listed) >= 6 && levenshteinAtMostOne(query, listed)
}

func pluralStem(word string) (string, bool) {
	if runeLen(word) >= 5 && strings.HasSuffix(word, "s") {
		return strings.TrimSuffix(word, "s"), true
	}
	return "", false
}

func levenshteinAtMostOne(leftText, rightText string) bool {
	left, right := []rune(leftText), []rune(rightText)
	delta := len(left) - len(right)
	if delta > 1 || delta < -1 {
		return false
	}
	if delta == 0 {
		mismatches := 0
		for i := range left {
			if left[i] != right[i] {
				mismatches++
				if mismatches > 1 {
					return false
				}
			}
		}
		return mismatches == 1
	}
	shorter, longer := left, right
	if len(left) > len(right) {
		shorter, longer = right, left
	}
	i, j, skipped := 0, 0, false
	for i < len(shorter) && j < len(longer) {
		switch {
		case shorter[i] == longer[j]:
			i++
			j++
		case skipped:
			return false
		default:
			skipped = true
			j++
		}
	}
	return true
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func nonEmpty(words []string) []string {
	out := words[:0:0]
	for _, word := range words {
		if word != "" {
			out = append(out, word)
		}
	}
	return out
}

// Security is what search reads from an app's security file.
type Security struct {
	FactBits int
	Keywords []string
}

// ParseSecurity reads the fact bits and keywords from a security file. Yes
// rows of searchable facts set bits; a missing row, a no, and every other
// fact leave the bit clear. The older dash-separated file has no keywords.
func ParseSecurity(text string) Security {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		var file struct {
			Facts    map[string]any `json:"facts"`
			Keywords []any          `json:"keywords"`
		}
		if json.Unmarshal([]byte(trimmed), &file) != nil {
			return Security{}
		}
		var out Security
		for key, value := range file.Facts {
			if yes, ok := value.(bool); ok && yes {
				out.FactBits |= pillByKey[key]
			}
		}
		for _, keyword := range file.Keywords {
			if s, ok := keyword.(string); ok {
				out.Keywords = append(out.Keywords, s)
			}
		}
		return out
	}
	return Security{FactBits: legacyFactBits(trimmed)}
}

// legacyFactBits reads the CSV head of the older file: rows, then two ---
// lines.
func legacyFactBits(text string) int {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	mark := -1
	marks := 0
	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			if mark < 0 {
				mark = i
			}
			marks++
		}
	}
	if marks < 2 || !strings.HasPrefix(strings.TrimSpace(lines[0]), `"`) {
		return 0
	}
	set := 0
	for _, line := range lines[:mark] {
		record := csvFields(line)
		if len(record) < 2 {
			continue
		}
		key := strings.TrimSpace(record[0])
		if strings.TrimSpace(record[1]) == "yes" {
			set |= pillByKey[key]
		}
	}
	return set
}

func csvFields(line string) []string {
	var fields []string
	var field strings.Builder
	quoted := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quoted && c == '"' && i+1 < len(line) && line[i+1] == '"':
			field.WriteByte('"')
			i++
		case c == '"':
			quoted = !quoted
		case !quoted && c == ',':
			fields = append(fields, field.String())
			field.Reset()
		case c != '\r':
			field.WriteByte(c)
		}
	}
	return append(fields, field.String())
}
