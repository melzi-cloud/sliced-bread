package main

import (
	"encoding/json"
	"flag"
	"fmt"
)

func (c *client) runAlerts(fs *flag.FlagSet, rest []string, jsonOut *bool) error {
	if len(rest) > 0 && (rest[0] == "test" || rest[0] == "configure") {
		return c.humanOnly("Alert webhook settings", "/settings")
	}
	if _, err := parse(fs, rest); err != nil {
		return err
	}
	b, _, err := c.call("GET", "/v1/alerts", nil)
	if err != nil {
		return err
	}
	if *jsonOut {
		c.print(b)
		return nil
	}
	var v struct {
		Alerts []struct {
			Severity, Title, Detail, Hint, Since string
		} `json:"alerts"`
		Webhook string `json:"webhook"`
	}
	json.Unmarshal(b, &v)
	if len(v.Alerts) == 0 {
		fmt.Println("no alerts")
	}
	for _, a := range v.Alerts {
		fmt.Printf("%s: %s (since %s)\n", a.Severity, a.Title, a.Since)
		if a.Detail != "" {
			fmt.Printf("  %s\n", a.Detail)
		}
		if a.Hint != "" {
			fmt.Printf("  next: %s\n", a.Hint)
		}
	}
	fmt.Println("webhook:", v.Webhook)
	return nil
}
