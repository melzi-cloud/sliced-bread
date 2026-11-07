package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
)

const membersUsage = `usage:
  sb members                                   members, pending invites and whether members can approve
  sb members invite <email> [--admin]          admin token: create a one-time invite link (send it yourself)
  sb members remove <email>                    admin token: remove a member (ends their sessions) or revoke an invite
  sb members role <email> admin|member         admin token
  sb members settings --members-can-approve true|false   admin token: may members approve risky changes?`

func (c *client) runMembers(fs *flag.FlagSet, rest []string, jsonOut *bool) error {
	sub := ""
	if len(rest) > 0 && (rest[0] == "invite" || rest[0] == "remove" || rest[0] == "role" || rest[0] == "settings") {
		sub, rest = rest[0], rest[1:]
	}
	admin := fs.Bool("admin", false, "")
	canApprove := fs.String("members-can-approve", "", "")
	pos, err := parse(fs, rest)
	if err != nil {
		return err
	}
	var b []byte
	switch sub {
	case "invite":
		if len(pos) != 1 {
			return &exitErr{exitUsage, membersUsage}
		}
		role := "member"
		if *admin {
			role = "admin"
		}
		b, _, err = c.call("POST", "/v1/members", map[string]string{"email": pos[0], "role": role})
		if err == nil && !*jsonOut {
			var v struct{ InviteURL, Expires, Note string }
			json.Unmarshal(b, &v)
			fmt.Printf("invite for %s (%s), valid until %s:\n%s\n%s\n", pos[0], role, v.Expires, v.InviteURL, v.Note)
			return nil
		}
	case "remove":
		if len(pos) != 1 {
			return &exitErr{exitUsage, membersUsage}
		}
		b, _, err = c.call("DELETE", "/v1/members/"+url.PathEscape(pos[0]), nil)
	case "role":
		if len(pos) != 2 {
			return &exitErr{exitUsage, membersUsage}
		}
		b, _, err = c.call("POST", "/v1/members/"+url.PathEscape(pos[0])+"/role", map[string]string{"role": pos[1]})
	case "settings":
		if *canApprove != "true" && *canApprove != "false" {
			return &exitErr{exitUsage, membersUsage}
		}
		b, _, err = c.call("PUT", "/v1/members/settings", map[string]bool{"membersCanApprove": *canApprove == "true"})
	default:
		b, _, err = c.call("GET", "/v1/members", nil)
		if err == nil && !*jsonOut {
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
			return nil
		}
	}
	if err != nil {
		return err
	}
	c.print(b)
	return nil
}
