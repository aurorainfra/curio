package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/fatih/color"
	"github.com/urfave/cli/v2"
	"golang.org/x/term"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/aurora/webauth"
	"github.com/filecoin-project/curio/cmd/curio/internal/translations"
	"github.com/filecoin-project/curio/deps"
	"github.com/filecoin-project/curio/deps/config"
	"github.com/filecoin-project/curio/harmony/harmonydb"
)

var configManageUsersCmd = &cli.Command{
	Name:      "manage-users",
	Usage:     translations.T("Manage web UI users (Aurora.WebUI.Users) in a config layer, interactively or with flags"),
	ArgsUsage: translations.T("[layer name, default: base]"),
	Flags: []cli.Flag{
		&cli.BoolFlag{Name: "list", Usage: translations.T("list the layer's users and exit")},
		&cli.StringFlag{Name: "add", Usage: translations.T("add a user (prompts for the password)")},
		&cli.StringFlag{Name: "passwd", Usage: translations.T("set a new password for a user")},
		&cli.StringFlag{Name: "disable", Usage: translations.T("disable a user")},
		&cli.StringFlag{Name: "enable", Usage: translations.T("enable a disabled user")},
		&cli.StringFlag{Name: "remove", Usage: translations.T("remove a user")},
		&cli.BoolFlag{Name: "password-stdin", Usage: translations.T("read the password from the first line of stdin instead of prompting")},
	},
	Action: func(cctx *cli.Context) error {
		layer := cctx.Args().First()
		if layer == "" {
			layer = "base"
		}
		db, err := deps.MakeDB(cctx)
		if err != nil {
			return err
		}
		ctx := cctx.Context

		orig, exists, err := readLayer(ctx, db, layer)
		if err != nil {
			return err
		}
		users, set, err := webauth.LayerUsers(orig)
		if err != nil {
			return xerrors.Errorf("layer %s: %w", layer, err)
		}
		if !set {
			users = nil
		}

		in := bufio.NewReader(os.Stdin)
		pw := passwordReader(in, cctx.Bool("password-stdin"))

		if cctx.Bool("list") {
			printUsers(layer, users, set)
			return nil
		}

		changed := false
		flagged := false
		for _, op := range []struct {
			flag string
			fn   func(*[]config.AuroraWebUser, string, func(string) (string, error)) error
		}{
			{"add", addUser}, {"passwd", passwdUser}, {"disable", disableUser(true)}, {"enable", disableUser(false)}, {"remove", removeUser},
		} {
			if name := cctx.String(op.flag); name != "" {
				flagged = true
				if err := op.fn(&users, name, pw); err != nil {
					return err
				}
				changed = true
			}
		}
		if !flagged {
			changed, err = interactiveUsers(layer, &users, set, in, pw)
			if err != nil {
				return err
			}
		}
		if !changed {
			fmt.Println("No changes.")
			return nil
		}
		return writeUsers(ctx, db, layer, orig, exists, users)
	},
}

func readLayer(ctx context.Context, db *harmonydb.DB, layer string) (string, bool, error) {
	var text string
	err := db.QueryRow(ctx, `SELECT config FROM harmony_config WHERE title = $1`, layer).Scan(&text)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return "", false, nil
		}
		return "", false, xerrors.Errorf("reading layer %s: %w", layer, err)
	}
	return text, true, nil
}

func printUsers(layer string, users []config.AuroraWebUser, set bool) {
	if !set || len(users) == 0 {
		fmt.Printf("Layer %s defines no web UI users.\n", color.GreenString(layer))
		return
	}
	fmt.Printf("Web UI users in layer %s:\n", color.GreenString(layer))
	for _, u := range users {
		state := color.GreenString("enabled")
		if u.Disabled {
			state = color.YellowString("disabled")
		}
		fmt.Printf("  %-24s %s\n", u.Name, state)
	}
}

func passwordReader(in *bufio.Reader, fromStdin bool) func(string) (string, error) {
	return func(name string) (string, error) {
		if fromStdin || !term.IsTerminal(int(os.Stdin.Fd())) {
			l, err := in.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return "", err
			}
			return strings.TrimRight(l, "\r\n"), nil
		}
		fmt.Printf("Password for %s: ", name)
		p1, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		fmt.Print("Repeat password: ")
		p2, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		if string(p1) != string(p2) {
			return "", errors.New("passwords do not match")
		}
		return string(p1), nil
	}
}

func findUser(users []config.AuroraWebUser, name string) int {
	return slices.IndexFunc(users, func(u config.AuroraWebUser) bool { return u.Name == name })
}

func addUser(users *[]config.AuroraWebUser, name string, pw func(string) (string, error)) error {
	if !webauth.ValidUserName(name) {
		return xerrors.Errorf("invalid user name %q: use letters, digits and _ . @ -", name)
	}
	if findUser(*users, name) >= 0 {
		return xerrors.Errorf("user %s already exists; use --passwd to change the password", name)
	}
	p, err := pw(name)
	if err != nil {
		return err
	}
	h, err := webauth.HashPassword(p)
	if err != nil {
		return err
	}
	*users = append(*users, config.AuroraWebUser{Name: name, PasswordBcrypt: h})
	return nil
}

func passwdUser(users *[]config.AuroraWebUser, name string, pw func(string) (string, error)) error {
	i := findUser(*users, name)
	if i < 0 {
		return xerrors.Errorf("no user %s in this layer", name)
	}
	p, err := pw(name)
	if err != nil {
		return err
	}
	h, err := webauth.HashPassword(p)
	if err != nil {
		return err
	}
	(*users)[i].PasswordBcrypt = h
	return nil
}

func disableUser(disabled bool) func(*[]config.AuroraWebUser, string, func(string) (string, error)) error {
	return func(users *[]config.AuroraWebUser, name string, _ func(string) (string, error)) error {
		i := findUser(*users, name)
		if i < 0 {
			return xerrors.Errorf("no user %s in this layer", name)
		}
		(*users)[i].Disabled = disabled
		return nil
	}
}

func removeUser(users *[]config.AuroraWebUser, name string, _ func(string) (string, error)) error {
	i := findUser(*users, name)
	if i < 0 {
		return xerrors.Errorf("no user %s in this layer", name)
	}
	*users = slices.Delete(*users, i, i+1)
	return nil
}

func interactiveUsers(layer string, users *[]config.AuroraWebUser, set bool, in *bufio.Reader, pw func(string) (string, error)) (bool, error) {
	changed := false
	ask := func(prompt string) (string, error) {
		fmt.Print(prompt)
		l, err := in.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		if errors.Is(err, io.EOF) && l == "" {
			return "q", nil
		}
		return strings.TrimSpace(l), nil
	}
	for {
		fmt.Println()
		printUsers(layer, *users, set || changed)
		action, err := ask("[a]dd  [p]assword  [d]isable  [e]nable  [r]emove  [w]rite and exit  [q]uit without saving: ")
		if err != nil {
			return false, err
		}
		var fn func(*[]config.AuroraWebUser, string, func(string) (string, error)) error
		switch strings.ToLower(action) {
		case "a":
			fn = addUser
		case "p":
			fn = passwdUser
		case "d":
			fn = disableUser(true)
		case "e":
			fn = disableUser(false)
		case "r":
			fn = removeUser
		case "w":
			return changed, nil
		case "q":
			if changed {
				fmt.Println("Discarding changes.")
			}
			return false, nil
		default:
			continue
		}
		name, err := ask("User name: ")
		if err != nil {
			return false, err
		}
		if name == "" {
			continue
		}
		if err := fn(users, name, pw); err != nil {
			fmt.Println(color.RedString("Error: %s", err))
			continue
		}
		changed = true
	}
}

// writeUsers stores the layer with its users replaced. Only the users
// entries change; the rest of the layer text is kept as it was. The write
// fails if the layer changed since it was read.
func writeUsers(ctx context.Context, db *harmonydb.DB, layer, orig string, exists bool, users []config.AuroraWebUser) error {
	text, err := webauth.SetLayerUsers(orig, users)
	if err != nil {
		return err
	}
	enabled := 0
	for _, u := range users {
		if !u.Disabled {
			enabled++
		}
	}
	if enabled == 0 && layer == "base" {
		fmt.Println(color.YellowString("Warning: base defines no enabled web UI users; nodes without users in another layer refuse all web UI access."))
	}

	comm, err := db.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
		var cur string
		err := tx.QueryRow(`SELECT config FROM harmony_config WHERE title = $1`, layer).Scan(&cur)
		switch {
		case err != nil && strings.Contains(err.Error(), "no rows"):
			if exists {
				return false, xerrors.Errorf("layer %s was removed while editing", layer)
			}
			if _, err := tx.Exec(`INSERT INTO harmony_config (title, config) VALUES ($1, $2)`, layer, text); err != nil {
				return false, err
			}
			return true, nil
		case err != nil:
			return false, err
		}
		if !exists || cur != orig {
			return false, xerrors.Errorf("layer %s changed while editing; run the command again", layer)
		}
		if _, err := tx.Exec(`INSERT INTO harmony_config_history (title, config) VALUES ($1, $2)`, layer, cur); err != nil {
			return false, xerrors.Errorf("saving history: %w", err)
		}
		if _, err := tx.Exec(`UPDATE harmony_config SET config = $1 WHERE title = $2`, text, layer); err != nil {
			return false, err
		}
		return true, nil
	}, harmonydb.OptionRetry())
	if err != nil {
		return err
	}
	if !comm {
		return xerrors.New("layer update was not committed")
	}
	fmt.Printf("%s Updated web UI users in layer %s. Running nodes pick the change up without a restart.\n", color.YellowString(">"), color.GreenString(layer))
	if layer != "base" {
		fmt.Println(color.YellowString("Note: users in a layer replace the users of the layers below it on nodes running that layer."))
	}
	return nil
}
