package main

import (
	"fmt"
	"strconv"
	"strings"
)

type rechargePackage struct {
	Credits int `json:"credits"`
	BDT     int `json:"bdt"` // 1 credit = 1 BDT
}

type config struct {
	Port            string
	DatabaseURL     string
	GoogleClientID  string
	AdminEmails     map[string]bool
	Prices          map[string]int
	FrontendOrigin  string
	DeviceKey       string
	RefundOnTimeout bool
	SessionDays     int
	Packages        []rechargePackage
}

func parseAdminEmails(s string) map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Split(s, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			m[e] = true
		}
	}
	return m
}

func parsePackages(s string) ([]rechargePackage, error) {
	var pkgs []rechargePackage
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("RECHARGE_PACKAGES: bad amount %q", p)
		}
		pkgs = append(pkgs, rechargePackage{Credits: n, BDT: n})
	}
	return pkgs, nil
}

func parsePrice(get func(string) string, key, def string) (int, error) {
	v := def
	if x := strings.TrimSpace(get(key)); x != "" {
		v = x
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", key, v)
	}
	return n, nil
}

func parseBool(s string, def bool) bool {
	if b, err := strconv.ParseBool(strings.TrimSpace(s)); err == nil {
		return b
	}
	return def
}

// loadConfig reads the accounts/credits settings. Device/timing settings
// (UNLOCK_SECONDS, STATUS_STALE_SECONDS, TEMP_TABLE) are still handled in main.
func loadConfig(get func(string) string) (*config, error) {
	or := func(k, def string) string {
		if v := strings.TrimSpace(get(k)); v != "" {
			return v
		}
		return def
	}
	c := &config{
		Port:            or("PORT", "8080"),
		DatabaseURL:     get("DATABASE_URL"),
		GoogleClientID:  get("GOOGLE_CLIENT_ID"),
		AdminEmails:     parseAdminEmails(get("ADMIN_EMAILS")),
		FrontendOrigin:  or("FRONTEND_ORIGIN", "*"),
		DeviceKey:       get("DEVICE_KEY"),
		RefundOnTimeout: parseBool(get("REFUND_ON_TIMEOUT"), true),
		Prices:          map[string]int{},
	}
	if c.DatabaseURL == "" || c.GoogleClientID == "" {
		return nil, fmt.Errorf("DATABASE_URL and GOOGLE_CLIENT_ID are required")
	}
	var err error
	for drink, kv := range map[string][2]string{
		"tea": {"PRICE_TEA", "5"}, "coffee": {"PRICE_COFFEE", "10"}, "hotwater": {"PRICE_HOTWATER", "0"},
	} {
		if c.Prices[drink], err = parsePrice(get, kv[0], kv[1]); err != nil {
			return nil, err
		}
	}
	if c.SessionDays, err = strconv.Atoi(or("SESSION_DAYS", "30")); err != nil || c.SessionDays <= 0 {
		return nil, fmt.Errorf("SESSION_DAYS must be a positive integer")
	}
	if c.Packages, err = parsePackages(or("RECHARGE_PACKAGES", "50,100,200")); err != nil {
		return nil, err
	}
	return c, nil
}
