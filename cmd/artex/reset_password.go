package main

import (
	"bufio"
	"fmt"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	"golang.org/x/crypto/bcrypt"
	"io"
	"os"
	"strings"
)

func resetAdministratorPassword(dataDir string, input io.Reader) int {
	path, _, err := config.SQLitePath(dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if _, err = os.Stat(path); err != nil {
		fmt.Fprintln(os.Stderr, "existing SQLite database is required:", err)
		return 1
	}
	password, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	password = strings.TrimSuffix(strings.TrimSuffix(password, "\n"), "\r")
	if password == "" {
		fmt.Fprintln(os.Stderr, "password must not be empty")
		return 1
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	store, err := db.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer store.Close()
	if err = store.SetSetting("auth.password_hash", string(hash)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("已重置 ARTEX 管理员密码，无需重启服务。")
	return 0
}
