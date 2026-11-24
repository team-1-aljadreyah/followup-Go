package support

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode/utf8"
)

var baghdadLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Baghdad")
	if err != nil {
		return time.FixedZone("Asia/Baghdad", 3*3600)
	}
	return loc
}()

func WorkingLocation(tz string) *time.Location {
	if tz == "" || tz == "Asia/Baghdad" {
		return baghdadLoc
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return baghdadLoc
	}
	return loc
}

func BaghdadNow() time.Time {
	return time.Now().In(baghdadLoc)
}

func BaghdadToday() string {
	return BaghdadNow().Format("2006-01-02")
}

func ToBaghdadDate(t time.Time) string {
	return t.In(baghdadLoc).Format("2006-01-02")
}

func FormatISO(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format("2006-01-02T15:04:05Z")
	return &s
}

func DaysBetween(fromDay, toDay string) int {
	a, err1 := time.Parse("2006-01-02", fromDay)
	b, err2 := time.Parse("2006-01-02", toDay)
	if err1 != nil || err2 != nil {
		return 0
	}
	return int(b.Sub(a).Hours() / 24)
}

func PlannedRatio(start, end, today string) float64 {
	length := DaysBetween(start, end)
	if length <= 0 {
		return 1.0
	}
	elapsed := DaysBetween(start, today)
	return math.Min(1.0, math.Max(0.0, float64(elapsed)/float64(length)))
}

var dateRegex = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func IsRealDay(val string) bool {
	if !dateRegex.MatchString(val) {
		return false
	}
	t, err := time.Parse("2006-01-02", val)
	if err != nil {
		return false
	}
	return t.Format("2006-01-02") == val
}

func IsLeapYear(year int) bool {
	return (year%4 == 0 && year%100 != 0) || (year%400 == 0)
}

func BirthdayInYear(birthDate string, year int) string {
	if len(birthDate) < 5 {
		return fmt.Sprintf("%04d-01-01", year)
	}
	md := birthDate[len(birthDate)-5:]
	if md == "02-29" && !IsLeapYear(year) {
		return fmt.Sprintf("%04d-02-28", year)
	}
	return fmt.Sprintf("%04d-%s", year, md)
}

func NextBirthday(birthDate, day string) (string, int) {
	if len(day) < 4 {
		return "", 0
	}
	var year int
	fmt.Sscanf(day[:4], "%d", &year)
	date := BirthdayInYear(birthDate, year)
	if date < day {
		date = BirthdayInYear(birthDate, year+1)
	}
	return date, DaysBetween(day, date)
}

func ReminderForDay(birthDate, day string) (kind string, date string, ok bool) {
	d, days := NextBirthday(birthDate, day)
	if days == 0 {
		return "birthday_today", d, true
	}
	if days == 7 {
		return "birthday_soon", d, true
	}
	return "", "", false
}

func AgeOn(birthDate, day string) int {
	var y1, y2 int
	fmt.Sscanf(day[:4], "%d", &y1)
	fmt.Sscanf(birthDate[:4], "%d", &y2)
	years := y1 - y2
	if len(day) >= 5 && len(birthDate) >= 5 && day[5:] < birthDate[5:] {
		return years - 1
	}
	return years
}

func BirthDateProblem(value, today string) string {
	if !IsRealDay(value) {
		return "date"
	}
	if value > today {
		return "future"
	}
	age := AgeOn(value, today)
	if age < 16 {
		return "young"
	}
	if age > 80 {
		return "old"
	}
	return ""
}

func FormatDayForLocale(day, locale string) string {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return day
	}
	if locale == "ar" {
		months := map[time.Month]string{
			1: "كانون الثاني", 2: "شباط", 3: "آذار", 4: "نيسان",
			5: "أيار", 6: "حزيران", 7: "تموز", 8: "آب",
			9: "أيلول", 10: "تشرين الأول", 11: "تشرين الثاني", 12: "كانون الأول",
		}
		return fmt.Sprintf("%d %s", t.Day(), months[t.Month()])
	}
	return t.Format("Jan 2")
}

// PasswordProblem checks password rules matching PasswordRule::problem.
func PasswordProblem(pw string, min int) (string, map[string]any) {
	if pw == "" {
		return "api.password.required", nil
	}
	n := utf8.RuneCountInString(pw)
	if n < min {
		return "api.password.too_short", map[string]any{"min": min}
	}
	if n > 128 {
		return "api.password.too_long", nil
	}
	if strings.TrimSpace(pw) == "" {
		return "api.password.spaces_only", nil
	}
	return "", nil
}

var arabicTashkeelRegex = regexp.MustCompile("[\u064B-\u0652]")

func NormalizeSearch(text string) string {
	if text == "" {
		return ""
	}
	s := strings.ToLower(strings.TrimSpace(text))
	r := strings.NewReplacer(
		"أ", "ا",
		"إ", "ا",
		"آ", "ا",
		"ة", "ه",
		"ى", "ي",
	)
	s = r.Replace(s)
	return arabicTashkeelRegex.ReplaceAllString(s, "")
}

func SearchMatches(query string, fields ...string) bool {
	needle := NormalizeSearch(query)
	if needle == "" {
		return true
	}
	for _, f := range fields {
		if f != "" && strings.Contains(NormalizeSearch(f), needle) {
			return true
		}
	}
	return false
}

func IPMatches(ip, filter string) bool {
	f := strings.TrimSpace(filter)
	if f == "" {
		return true
	}
	if ip == "" {
		return false
	}
	if strings.HasSuffix(f, "*") {
		return strings.HasPrefix(ip, strings.TrimSuffix(f, "*"))
	}
	if strings.HasSuffix(f, ".") || strings.HasSuffix(f, ":") {
		return strings.HasPrefix(ip, f)
	}
	return ip == f || strings.HasPrefix(ip, f+".") || strings.HasPrefix(ip, f+":")
}

func RandomHex(byteLen int) string {
	b := make([]byte, byteLen)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

func RuneLen(s string) int {
	return utf8.RuneCountInString(s)
}
