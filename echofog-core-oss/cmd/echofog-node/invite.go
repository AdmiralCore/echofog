package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/network/access"
)

// The invitation link of a running node, over its local status endpoint:
//
//	echofog-node invite             print the link
//	echofog-node invite --rotate    make a new secret: the old link lets nobody new in
//	echofog-node invite --list      who joined with the link
//	echofog-node invite --remove ID take somebody off that list
//	echofog-node invite --new-secret print a fresh access secret (no node needed), for
//	                                 ECHOFOG_ACCESS_SECRET or a Docker/Kubernetes secret
//
// A request that changes something must carry the X-Echofog header: a web page in a browser on
// the same machine cannot set it without a preflight the endpoint never answers, so it cannot
// rotate the secret behind the operator's back.

const inviteHeader = "X-Echofog"

func registerInvite(mux *http.ServeMux, c *collector) {
	mux.HandleFunc("/invite", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			st, err := c.h.GetAccessStatus()
			if err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(st)
		case http.MethodPost:
			if r.Header.Get(inviteHeader) != "1" {
				http.Error(w, "missing "+inviteHeader+" header", http.StatusForbidden)
				return
			}
			var err error
			switch r.URL.Query().Get("do") {
			case "rotate":
				_, err = c.h.RotateAccessSecret()
			case "remove":
				err = c.h.RemoveJoinedPeer(r.URL.Query().Get("peer"))
			default:
				http.Error(w, "unknown action", http.StatusBadRequest)
				return
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, "ok\n")
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// inviteStatus mirrors api.AccessStatus.
type inviteStatus struct {
	Mode      string `json:"mode"`
	Link      string `json:"link"`
	LinkError string `json:"linkError"`
	Joined    []struct {
		PeerID    string `json:"peerId"`
		JoinedAt  int64  `json:"joinedAt"`
		Connected bool   `json:"connected"`
	} `json:"joined"`
}

func inviteCmd(args []string) int {
	f := flag.NewFlagSet("invite", flag.ExitOnError)
	addr := f.String("status-addr", getEnv("ECHOFOG_STATUS_ADDR", "127.0.0.1:7070"), "status endpoint of the running node")
	rotate := f.Bool("rotate", false, "make a new access secret: the old link lets nobody new in, those who joined stay")
	list := f.Bool("list", false, "list the peers who joined with the link")
	remove := f.String("remove", "", "take this peer ID off the list of those who joined")
	newSecret := f.Bool("new-secret", false, "print a fresh access secret for --access-secret / ECHOFOG_ACCESS_SECRET (needs no running node)")
	f.Usage = func() {
		fmt.Fprintln(f.Output(), "Usage: echofog-node invite [flags]\n\nPrints the invitation link of the running node: give it to the people who may use it.")
		f.PrintDefaults()
	}
	_ = f.Parse(args)
	if *newSecret {
		fmt.Println(access.EncodeSecret(access.NewSecret()))
		return 0
	}
	base := "http://" + *addr + "/invite"
	client := &http.Client{Timeout: 10 * time.Second}

	post := func(q url.Values) bool {
		req, _ := http.NewRequest(http.MethodPost, base+"?"+q.Encode(), nil)
		req.Header.Set(inviteHeader, "1")
		res, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "the node does not answer on %s: is it running (echofog-node run)? %v\n", *addr, err)
			return false
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
			fmt.Fprintf(os.Stderr, "refused: %s\n", strings.TrimSpace(string(msg)))
			return false
		}
		return true
	}
	switch {
	case *rotate:
		if !post(url.Values{"do": {"rotate"}}) {
			return 1
		}
		fmt.Println("New secret made: the old link lets nobody new in. The new link:")
	case *remove != "":
		if !post(url.Values{"do": {"remove"}, "peer": {*remove}}) {
			return 1
		}
		fmt.Println("Removed. To come back that peer needs the current link.")
		return 0
	}

	res, err := client.Get(base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "the node does not answer on %s: is it running (echofog-node run)? %v\n", *addr, err)
		return 1
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		fmt.Fprintf(os.Stderr, "%s\n", strings.TrimSpace(string(msg)))
		return 1
	}
	var st inviteStatus
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		fmt.Fprintln(os.Stderr, "unreadable answer from the node")
		return 1
	}
	if *list {
		if len(st.Joined) == 0 {
			fmt.Println("Nobody has joined with the link yet.")
			return 0
		}
		for _, j := range st.Joined {
			state := ""
			if j.Connected {
				state = "  connected"
			}
			fmt.Printf("%s  joined %s%s\n", j.PeerID, time.Unix(j.JoinedAt, 0).Format("2006-01-02 15:04"), state)
		}
		return 0
	}
	if st.Link == "" {
		fmt.Fprintf(os.Stderr, "no link yet: %s\n", st.LinkError)
		return 1
	}
	fmt.Println(st.Link)
	if st.Mode == "public" {
		fmt.Fprintln(os.Stderr, "(the node is public: the link carries no secret, it only tells where the node is)")
	}
	return 0
}
