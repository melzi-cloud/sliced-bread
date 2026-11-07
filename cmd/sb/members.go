package main

import (
	"encoding/json"
	"flag"
	"fmt"
)

func (c *client) runMembers(fs *flag.FlagSet, rest []string, jsonOut *bool) error {
	if len(rest) > 0 && (rest[0] == "invite" || rest[0] == "remove" || rest[0] == "role" || rest[0] == "settings") {
		return c.humanOnly("Inviting, removing and changing members", "/members")
	}
	if _, err := parse(fs, rest); err != nil {
		return err
	}
	b, _, err := c.call("GET", "/v1/members", nil)
	if err != nil {
		return err
	}
	if *jsonOut {
		c.print(b)
		return nil
	}
	var v struct {
		Members           []struct{ Email, Role string }
		Invites           []struct{ Email, Role, Expires string }
		MembersCanApprove bool
	}
	json.Unmarshal(b, &v)
	for _, m := range v.Members {
		fmt.Printf("%s  %s\n", m.Email, m.Role)
	}
	for _, i := range v.Invites {
		fmt.Printf("%s  invited as %s, until %s\n", i.Email, i.Role, i.Expires)
	}
	fmt.Println("members can approve risky changes:", v.MembersCanApprove)
	fmt.Println("manage members in the dashboard:", c.dashboardURL("/members"))
	return nil
}
