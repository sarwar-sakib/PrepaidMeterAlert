package nesco

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"

	"github.com/m4hi2/MeterAlertBot/internal/config"
	"github.com/m4hi2/MeterAlertBot/internal/datasources"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/net/html"
	"golang.org/x/time/rate"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

type Service struct {
	client  *http.Client
	limiter *rate.Limiter
	apiHits metric.Int64Counter
	cfg     config.NescoConfig
}

func NewService(cfg config.NescoConfig) *Service {
	jar, _ := cookiejar.New(nil)

	m := otel.Meter("meterbot/nesco")
	apiHits, _ := m.Int64Counter(
		"nesco.api.hit",
		metric.WithDescription("Number of successful NESCO balance fetches"),
	)

	return &Service{
		client: &http.Client{
			Timeout: cfg.Timeout,
			Jar:     jar,
		},
		limiter: rate.NewLimiter(rate.Limit(cfg.RateLimit), 1),
		apiHits: apiHits,
		cfg:     cfg,
	}
}

func (s *Service) GetBalance(ctx context.Context, id datasources.Identifier) (datasources.Balance, error) {
	if err := s.limiter.Wait(ctx); err != nil {
		return datasources.Balance{}, fmt.Errorf("rate limit wait: %w", err)
	}

	ctx = context.WithValue(ctx, datasources.CtxKeyDatasource, datasources.CtxDatasourceNesco)

	// 1. Visit homepage to establish session
	if err := s.visitHome(ctx); err != nil {
		return datasources.Balance{}, fmt.Errorf("visit home: %w", err)
	}

	// 2. Switch language to English
	if err := s.switchToEnglish(ctx); err != nil {
		return datasources.Balance{}, fmt.Errorf("switch language: %w", err)
	}

	// 3. Get CSRF token
	token, err := s.getCSRFToken(ctx)
	if err != nil {
		return datasources.Balance{}, fmt.Errorf("get csrf token: %w", err)
	}

	// 4. Post to fetch balance
	resp, err := s.fetchBalance(ctx, id.AccountNumber, token)
	if err != nil {
		return datasources.Balance{}, err
	}

	s.apiHits.Add(ctx, 1, metric.WithAttributes(attribute.String("nesco.api", "panel")))

	outID := id
	if resp.Data.MeterNo != "" {
		outID.MeterNumber = resp.Data.MeterNo
	}

	var balance float64
	if resp.Data.Balance != "" {
		balance, err = strconv.ParseFloat(strings.TrimSpace(resp.Data.Balance), 64)
		if err != nil {
			slog.ErrorContext(ctx, "invalid balance", "balance", resp.Data.Balance, "error", err)
			balance = 0
		}
	}

	return datasources.Balance{
		Identifier: outID,
		Balance:    balance,
	}, nil
}

func (s *Service) Name() string {
	return "nesco"
}

func (s *Service) visitHome(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BasePath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (s *Service) switchToEnglish(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BasePath+languageEn, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("language switch request: %w", err)
	}
	defer resp.Body.Close()
	return nil
}

func (s *Service) getCSRFToken(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BasePath+panelPath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", s.cfg.BasePath+languageEn) // mimic navigation

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Debug
	fmt.Printf("DEBUG: GET /pre/panel status: %d\n", resp.StatusCode)
	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)
	fmt.Printf("DEBUG: body length: %d\n", len(body))
	if len(body) > 0 {
		fmt.Printf("DEBUG: first 500 chars:\n%s\n", truncate(body, 500))
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("panel page status %d, body snippet: %s", resp.StatusCode, truncate(body, 200))
	}

	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("parse html: %w", err)
	}

	var token string
	var find func(*html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "meta" {
			var name, content string
			for _, attr := range n.Attr {
				if attr.Key == "name" {
					name = attr.Val
				}
				if attr.Key == "content" {
					content = attr.Val
				}
			}
			if name == "csrf-token" && content != "" {
				token = content
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)

	if token == "" {
		// Fallback: hidden input
		fallbackToken := extractTokenFromHiddenInput(body)
		if fallbackToken != "" {
			fmt.Printf("DEBUG: found token in hidden input: %s\n", fallbackToken)
			return fallbackToken, nil
		}
		return "", fmt.Errorf("csrf-token not found in response")
	}
	return token, nil
}

func extractTokenFromHiddenInput(body string) string {
	start := strings.Index(body, `name="_token"`)
	if start == -1 {
		return ""
	}
	valueIdx := strings.Index(body[start:], `value="`)
	if valueIdx == -1 {
		return ""
	}
	start += valueIdx + len(`value="`)
	end := strings.Index(body[start:], `"`)
	if end == -1 {
		return ""
	}
	return body[start : start+end]
}

func (s *Service) fetchBalance(ctx context.Context, custNo, token string) (*NescoBalanceResp, error) {
	form := url.Values{}
	form.Set(paramToken, token)
	form.Set(paramCustNo, custNo)
	form.Set(paramSubmit, submitRecharge)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.BasePath+panelPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", s.cfg.BasePath+panelPath)
	req.Header.Set("Origin", s.cfg.BasePath)

	slog.DebugContext(ctx, "nesco posting for balance", "cust_no", custNo)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upstream status %d, body: %s", resp.StatusCode, truncate(string(body), 200))
	}

	return parseBalancePage(ctx, resp.Body)
}

// parseBalancePage (if not in parse.go, keep it; otherwise remove)
func parseBalancePage(ctx context.Context, body io.Reader) (*NescoBalanceResp, error) {
	doc, err := html.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("parse response html: %w", err)
	}

	data := make(map[string]string)
	var currentLabel string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "label" {
				if n.FirstChild != nil {
					currentLabel = strings.TrimSpace(n.FirstChild.Data)
					currentLabel = strings.ReplaceAll(currentLabel, "\n", " ")
				}
			}
			if n.Data == "input" && currentLabel != "" {
				for _, attr := range n.Attr {
					if attr.Key == "value" {
						data[currentLabel] = strings.TrimSpace(attr.Val)
						currentLabel = ""
						break
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if len(data) == 0 {
		return nil, fmt.Errorf("no data extracted from NESCO response")
	}

	acc, ok1 := data[AccountNumber]
	meter, ok2 := data[MeterNumber]
	balanceStr, ok3 := data[Balance]
	if !ok1 || !ok2 || !ok3 {
		return nil, fmt.Errorf("missing required NESCO fields: account=%v meter=%v balance=%v", ok1, ok2, ok3)
	}

	return &NescoBalanceResp{
		Code: http.StatusOK,
		Data: struct {
			AccountNo string `json:"accountNo"`
			MeterNo   string `json:"meterNo"`
			Balance   string `json:"balance"`
		}{
			AccountNo: acc,
			MeterNo:   meter,
			Balance:   balanceStr,
		},
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
