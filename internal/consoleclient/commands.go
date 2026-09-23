// Package consoleclient implements the read-only service connection path for
// the console CLI clients. It does not build or initialize an application.
package consoleclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/gopact-ai/steve/internal/tui"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// top renders the read model in this terminal. It is a client of the running
// gateway's HTTP surface, not a second reader of the stores: one read model,
// two renderers, so the terminal and the browser cannot disagree.
func Top(args []string) error {
	flags := flag.NewFlagSet("top", flag.ContinueOnError)
	connectionFlags := addConsoleClientFlags(flags)
	refresh := flags.Duration("refresh", 5*time.Second, "redraw floor; changes also redraw immediately")
	once := flags.Bool("once", false, "print one frame and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	connection, err := connectionFlags.resolve(flags)
	if err != nil {
		return err
	}
	model := tui.New(tui.Config{URL: connection.URL, Token: connection.Token, Refresh: *refresh, CheckRedirect: checkConsoleRedirect})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		fmt.Print(model.Once(ctx))
		return nil
	}
	return model.Run(ctx)
}

// dash prints the dashboard URL of a running gateway. The page is served by
// the gateway itself, so there is no second process to keep alive.
func Dash(args []string) error {
	flags := flag.NewFlagSet("dash", flag.ContinueOnError)
	connectionFlags := addConsoleClientFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	connection, err := connectionFlags.resolve(flags)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.URL+"/state", nil)
	if err != nil {
		return err
	}
	if connection.Token != "" {
		req.Header.Set("Authorization", "Bearer "+connection.Token)
	}
	res, err := consoleHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("no gateway at %s — is `steve run` up? %w", connection.URL, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return answered("read model", connection.URL, res)
	}
	page := connection.URL
	if connection.Token != "" {
		page += "/?token=" + neturl.QueryEscape(connection.Token)
	}
	fmt.Println(page)
	return nil
}

// answered reports a response the command cannot use. A refused token is the
// one case the user can fix from here, so it names the flags that do.
func answered(surface, url string, res *http.Response) error {
	if res.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%s at %s refused the token; pass -config with the Hub's config, or -token", surface, url)
	}
	return fmt.Errorf("%s at %s answered %s", surface, url, res.Status)
}

// defaultReadModelURL is where `steve run` puts the read model unless the
// config says otherwise.
const defaultReadModelURL = "http://127.0.0.1:7710"

// say sends one line to a running gateway's console and prints the reply:
// the page's send box, from a shell.
func Say(args []string) error {
	flags := flag.NewFlagSet("say", flag.ContinueOnError)
	connectionFlags := addConsoleClientFlags(flags)
	conversation := flags.String("conversation", "console:main", "console conversation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	input := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if input == "" {
		return errors.New("usage: steve say [-config …] [-url …] [-token …] <text or /verb …>")
	}
	connection, err := connectionFlags.resolve(flags)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"conversation": *conversation, "input": input}) // a map of strings always encodes
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, connection.URL+"/console/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if connection.Token != "" {
		req.Header.Set("Authorization", "Bearer "+connection.Token)
	}
	res, err := consoleHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("no gateway at %s — is `steve run` up? %w", connection.URL, err)
	}
	defer res.Body.Close()
	var out struct {
		Error string `json:"error"`
		Reply struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"reply"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return answered("console", connection.URL, res)
	}
	if res.StatusCode != http.StatusOK {
		if out.Error != "" {
			return errors.New(out.Error)
		}
		return answered("console", connection.URL, res)
	}
	if out.Reply.Title != "" {
		fmt.Println("== " + out.Reply.Title)
	}
	fmt.Println(out.Reply.Text)
	if out.Error != "" {
		return errors.New(out.Error)
	}
	return nil
}
