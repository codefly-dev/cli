package cmd

import (
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/platform"
	"github.com/spf13/cobra"
)

// LoginCmd represents the login command
var LoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Login to codefly platform",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		workspace := common.Workspace(ctx)

		token, err := platform.LoadToken(ctx, workspace)

		cli.ExitOnError(err, "Cannot login service")

		err = platform.Login(ctx, token)

		cli.ExitOnError(err, "Cannot login service")
		//token, err := loadToken(ctx)
		//if err != nil {
		//	cli.Error("Cannot load token")
		//}

		//err := login(ctx)
		//cli.ExitOnError(err, "Cannot login service")
	},
}

//
//func login(ctx context.Context) error {
//	w := wool.Get(ctx).In("login")
//	deviceCodeResp, err := requestDeviceCode(ctx)
//	if err != nil {
//		return w.Wrapf(err, "cannot request device code")
//	}
//	if deviceCodeResp.VerificationURIComplete == "" {
//		return w.NewError("cannot get verification URL")
//	}
//	_, err = url.Parse(deviceCodeResp.VerificationURIComplete)
//	if err != nil {
//		return w.Wrapf(err, "cannot parse verification URL")
//	}
//	fmt.Printf("Please go to %s and enter the code %s\n", deviceCodeResp.VerificationURI, deviceCodeResp.UserCode)
//	err = open.Run(deviceCodeResp.VerificationURIComplete)
//	if err != nil {
//		return w.Wrapf(err, "cannot open verification URL")
//	} // Optionally open the verification URL automatically
//
//	token := pollForToken(deviceCodeResp.DeviceCode)
//	fmt.Printf("Access Token: %s\n", token.AccessToken)
//	return nil
//}
//
//func init() {
//	clientId = "7oaxyVaAVCXipvNGHxycXnp2KoPfqhHT"
//	deviceCodeURL = "https://dev-4c24vdpgjj3eyqmy.us.auth0.com/oauth/device/code"
//	tokenURL = "https://dev-4c24vdpgjj3eyqmy.us.auth0.com/oauth/token"
//	audience = "https://codefly.ai" // Optional, depends on your Auth0 setup
//}
//
//var (
//	clientId      string
//	deviceCodeURL string
//	tokenURL      string
//	audience      string
//)
//
//const (
//	grantType       = "urn:ietf:params:oauth:grant-type:device_code"
//	pollingInterval = 5 // In seconds, adjust based on the interval suggested by Auth0 in the device code response
//)
//
//type DeviceCodeResponse struct {
//	DeviceCode              string `json:"device_code"`
//	UserCode                string `json:"user_code"`
//	VerificationURI         string `json:"verification_uri"`
//	VerificationURIComplete string `json:"verification_uri_complete"`
//	ExpiresIn               int    `json:"expires_in"`
//	Interval                int    `json:"interval"`
//}
//
//type TokenResponse struct {
//	AccessToken string `json:"access_token"`
//	TokenType   string `json:"token_type"`
//	Error       string `json:"error"`
//}
//
//func requestDeviceCode(ctx context.Context) (*DeviceCodeResponse, error) {
//	w := wool.Get(ctx).In("requestDeviceCode")
//	payload := map[string]string{
//		"client_id": clientId,
//		"audience":  audience,
//	}
//	payloadBytes, err := json.Marshal(payload)
//	if err != nil {
//		return nil, w.Wrapf(err, "cannot marshal payload")
//	}
//	resp, err := http.Post(deviceCodeURL, "application/json", bytes.NewBuffer(payloadBytes))
//	if err != nil {
//		return nil, w.Wrapf(err, "cannot post request")
//	}
//	defer func(Body io.ReadCloser) {
//		err = Body.Close()
//		if err != nil {
//			w.Warn("cannot close body")
//		}
//	}(resp.Body)
//
//	var result DeviceCodeResponse
//	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
//		return nil, w.Wrapf(err, "cannot decode response")
//	}
//
//	return &result, nil
//}
//
//func pollForToken(deviceCode string) TokenResponse {
//	payload := map[string]string{
//		"grant_type":  grantType,
//		"device_code": deviceCode,
//		"client_id":   clientId,
//	}
//	for {
//		payloadBytes, _ := json.Marshal(payload)
//		resp, err := http.Post(tokenURL, "application/json", bytes.NewBuffer(payloadBytes))
//		if err != nil {
//			panic(err)
//		}
//
//		var tokenResp TokenResponse
//		body, _ := io.ReadAll(resp.Body)
//		_ = resp.Body.Close()
//
//		if err := json.Unmarshal(body, &tokenResp); err != nil {
//			panic(err)
//		}
//
//		if tokenResp.AccessToken != "" {
//			return tokenResp
//		} else if tokenResp.Error == "authorization_pending" {
//			fmt.Println("Authorization pending. Waiting before trying again...")
//			time.Sleep(time.Duration(pollingInterval) * time.Second)
//			continue
//		} else {
//			panic(fmt.Sprintf("Failed to get token: %s", tokenResp.Error))
//		}
//	}
//}
