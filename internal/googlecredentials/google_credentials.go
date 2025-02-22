// MIT License
//
// Copyright (c) 2024 Matheus Pimenta
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package googlecredentials

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"golang.org/x/oauth2/google"
)

type (
	Config struct {
		opts ConfigOptions
	}

	ConfigOptions struct {
		WorkloadIdentityProvider string
	}
)

var workloadIdentityProviderRegex = regexp.MustCompile(`^projects/\d+/locations/global/workloadIdentityPools/([^/]+)/providers/[^/]+$`)

func AccessScopes() []string {
	return []string{
		"https://www.googleapis.com/auth/cloud-platform",
		"https://www.googleapis.com/auth/userinfo.email",
	}
}

func NewConfig(opts ConfigOptions) (*Config, string, error) {
	if !workloadIdentityProviderRegex.MatchString(opts.WorkloadIdentityProvider) {
		return nil, "", fmt.Errorf("workload identity provider name does not match pattern %s",
			workloadIdentityProviderRegex.String())
	}
	workloadIdentityPool := workloadIdentityProviderRegex.FindStringSubmatch(opts.WorkloadIdentityProvider)[1]
	return &Config{opts}, workloadIdentityPool, nil
}

func (c *Config) Get(ctx context.Context, credFile string, googleServiceAccountEmail *string) (*google.Credentials, error) {
	conf := map[string]any{
		"universe_domain":    "googleapis.com",
		"type":               "external_account",
		"audience":           c.WorkloadIdentityProviderAudience(),
		"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
		"token_url":          "https://sts.googleapis.com/v1/token",
		"credential_source": map[string]any{
			"file": credFile,
			"format": map[string]string{
				"type": "text",
			},
		},
	}

	if googleServiceAccountEmail != nil {
		conf["service_account_impersonation_url"] = fmt.Sprintf(
			"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateAccessToken",
			*googleServiceAccountEmail)
	} else {
		conf["token_info_url"] = "https://sts.googleapis.com/v1/introspect"
	}

	b, err := json.Marshal(conf)
	if err != nil {
		return nil, fmt.Errorf("error marshaling google credentials config to json: %w", err)
	}
	creds, err := google.CredentialsFromJSON(ctx, b, AccessScopes()...)
	if err != nil {
		return nil, fmt.Errorf("error creating google credentials from json: %w", err)
	}
	return creds, nil
}

func (c *Config) WorkloadIdentityProviderAudience() string {
	return fmt.Sprintf("//iam.googleapis.com/%s", c.opts.WorkloadIdentityProvider)
}
