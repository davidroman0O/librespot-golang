package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"
)

type OAuth struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string
}

func GetOauthAccessToken(code string, redirectUri string, clientId string, clientSecret string) (*OAuth, error) {
	val := url.Values{}
	val.Set("grant_type", "authorization_code")
	val.Set("code", code)
	val.Set("redirect_uri", redirectUri)
	val.Set("client_id", clientId)
	val.Set("client_secret", clientSecret)

	resp, err := http.PostForm("https://accounts.spotify.com/api/token", val)
	if err != nil {
		// Retry since there is an nginx bug that causes http2 streams to get
		// an initial REFUSED_STREAM response
		// https://github.com/curl/curl/issues/804
		resp, err = http.PostForm("https://accounts.spotify.com/api/token", val)
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	auth := OAuth{}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(body, &auth)
	if err != nil {
		return nil, err
	}
	if auth.Error != "" {
		return nil, fmt.Errorf("error getting token %v", auth.Error)
	}
	return &auth, nil
}

func GetOAuthUrl(clientId string) string {
	return "https://accounts.spotify.com/authorize?" +
		"client_id=" + clientId +
		"&response_type=code" +
		"&redirect_uri=http://localhost:8888/callback" +
		"&scope=streaming"
}

func getOAuthToken(clientId string, clientSecret string, cb func(url string)) (*OAuth, error) {
	ch := make(chan OAuth)
	mux := http.NewServeMux()

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		params := r.URL.Query()
		auth, err := GetOauthAccessToken(params.Get("code"), "http://localhost:8888/callback", clientId, clientSecret)
		if err != nil {
			fmt.Fprintf(w, "Error getting token %q", err)
			return
		}
		fmt.Fprintf(w, "Got token, loggin in")
		ch <- *auth
	})

	server := &http.Server{
		Addr:    ":8888",
		Handler: mux,
	}

	go func() {
		go func() {
			<-time.After(time.Second)
			cb(GetOAuthUrl(clientId))
		}()
		// log.Fatal(http.ListenAndServe(":8888", nil))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("ListenAndServe error: %v\n", err)
		}
	}()

	oauth := <-ch

	if err := server.Shutdown(context.TODO()); err != nil {
		fmt.Printf("Shutdown error: %v\n", err)
		return nil, err
	}

	return &oauth, nil
}
