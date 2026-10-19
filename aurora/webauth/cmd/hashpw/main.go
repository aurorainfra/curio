// Command hashpw prints the bcrypt hash of a password for
// Aurora.WebUI.Users[].PasswordBcrypt. The password is read from stdin.
//
//	go run ./aurora/webauth/cmd/hashpw
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	fmt.Fprint(os.Stderr, "password: ")
	pw, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && pw == "" {
		fmt.Fprintln(os.Stderr, "reading password:", err)
		os.Exit(1)
	}
	pw = strings.TrimRight(pw, "\r\n")
	if len(pw) < 12 {
		fmt.Fprintln(os.Stderr, "use at least 12 characters")
		os.Exit(1)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(h))
}
