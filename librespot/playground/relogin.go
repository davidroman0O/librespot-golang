package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/trace"
	"time"

	"github.com/avast/retry-go/v3"
	"github.com/librespot-org/librespot-golang/librespot/core"
	"github.com/pkg/browser"
)

// Program to test reconnection to find that goroutine leak
func main() {

	credentials := []struct {
		ClientID string
		SecretID string
		Name     string
		Username string
	}{
		// FILL YOUR CREDENTIALS HERE
	}

	// initial
	for _, credential := range credentials {
		var err error
		var session *core.Session
		if err := retry.Do(func() error {
			fmt.Println("Logging in with oauth", credential.Name, credential.Username)
			session, err = core.LoginOAuth(credential.Username, credential.ClientID, credential.SecretID, func(url string) {
				if err := browser.OpenURL(url); err != nil {
					log.Printf("Failed to open browser: %v", err)
				}
			})
			if err != nil {
				fmt.Println("Login error", err)
				return err
			}
			return nil
		}, retry.Attempts(3), retry.Delay(time.Second*2)); err != nil {
			panic(err)
		}

		fmt.Println("logged into", credential.Name, session.Username())

		blobFile := filepath.Join("./", credential.Name+".blob")
		if err := os.WriteFile(blobFile, session.ReusableAuthBlob(), 0644); err != nil {
			panic(err)
		}
		<-time.After(time.Second / 2)
	}

	credentialToUsername := map[string]string{}
	sessions := map[string]*core.Session{}
	for _, credential := range credentials {
		blobFile := filepath.Join("./", credential.Name+".blob")
		blobData, err := os.ReadFile(blobFile)
		if err != nil {
			panic(err)
		}
		var session *core.Session
		if err := retry.Do(func() error {
			fmt.Println("Logging in with saved blob", credential.Name, credential.Username, len(blobData))
			session, err = core.LoginSaved(credential.Username, blobData, credential.Name)
			if err != nil {
				fmt.Println("Login error", err)
				return err
			}
			return nil
		}, retry.Attempts(3), retry.Delay(time.Second*2)); err != nil {
			panic(err)
		}

		sessions[credential.Name] = session
		credentialToUsername[credential.Name] = credential.Username
	}

	f, err := os.Create("trace.out")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	if err := trace.Start(f); err != nil {
		log.Fatal(err)
	}
	defer trace.Stop()

	for {
		for name, session := range sessions {
			fmt.Println("reconnection to session", name)
			fmt.Println("\tdeconnection to session", name)
			session.Disconnection()
			<-time.After(1 * time.Second)
			blobFile := filepath.Join("./", name+".blob")
			blobData, err := os.ReadFile(blobFile)
			if err != nil {
				panic(err)
			}
			var session *core.Session
			if err := retry.Do(func() error {
				fmt.Println("Logging in with saved blob", name, credentialToUsername[name], len(blobData))
				session, err = core.LoginSaved(credentialToUsername[name], blobData, name)
				if err != nil {
					fmt.Println("Login error", err)
					return err
				}
				return nil
			}, retry.Attempts(3), retry.Delay(time.Second*2)); err != nil {
				panic(err)
			}

			sessions[name] = session
		}
	}
}
