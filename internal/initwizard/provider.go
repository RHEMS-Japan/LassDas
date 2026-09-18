package initwizard

import (
	"errors"
	"net/url"
	"strings"
)

// ModelProvider is one connection target for the roles' models. The engine
// talks OpenAI-compatible chat completions and holds no provider credential
// of its own, so a provider is a name and a base URL - there is no adapter
// to write, and one that is not on this list is reachable through the
// "other" answer.
type ModelProvider struct {
	Name    string
	BaseURL string
	Detail  string
}

// ModelProviders are the ones offered by name. Both are marketplaces that
// front several vendors, which is why the different-vendor rules look at the
// vendor written against each role rather than the host they are reached
// through: behind a shared gateway every vendor is the same host.
var ModelProviders = []ModelProvider{
	{
		Name:    "OpenRouter",
		BaseURL: openRouterBaseURL,
		Detail:  "モデル名が 会社/モデル の形なので、提供会社が名前から埋まる",
	},
	{
		Name:    "Cheaper Inference",
		BaseURL: "https://api.cheaperinference.com/v1",
		Detail:  "モデル名に会社名が入らないので、役ごとに提供会社を自分で書く",
	},
}

// ProviderName is the name to show for a base URL: the known provider's, or
// the host, so a message never says "OpenRouter" about somewhere else.
func ProviderName(baseURL string) string {
	for _, provider := range ModelProviders {
		if provider.BaseURL == baseURL {
			return provider.Name
		}
	}
	if parsed, err := url.Parse(baseURL); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return baseURL
}

// VendorDerivable reports whether the provider names models vendor/model, so
// the setup can say up front that every role needs its vendor written out.
func VendorDerivable(baseURL string) bool { return baseURL == openRouterBaseURL }

// CheckModelBaseURL refuses a connection target that cannot be one: the
// engine sends the key to it, so a http:// or a host-less value is a
// credential sent somewhere unintended, and a trailing slash produces
// //chat/completions on a provider that is strict about paths.
func CheckModelBaseURL(baseURL string) error {
	if strings.TrimSpace(baseURL) != baseURL || baseURL == "" {
		return errors.New("モデルの接続先が空です")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("モデルの接続先は https://<ホスト>/… の形にしてください: " + baseURL)
	}
	if strings.HasSuffix(baseURL, "/") {
		return errors.New("モデルの接続先の末尾の / を外してください: " + baseURL)
	}
	return nil
}
