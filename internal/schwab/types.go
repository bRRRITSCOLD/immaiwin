package schwab

// OAuth token response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"` // seconds
	TokenType    string `json:"token_type"`
}

// Options chain response from GET /marketdata/v1/chains.
type chainResponse struct {
	Symbol         string                                     `json:"symbol"`
	Status         string                                     `json:"status"`
	CallExpDateMap map[string]map[string][]chainOptionRaw     `json:"callExpDateMap"`
	PutExpDateMap  map[string]map[string][]chainOptionRaw     `json:"putExpDateMap"`
}

type chainOptionRaw struct {
	Symbol          string  `json:"symbol"`
	PutCall         string  `json:"putCall"`   // "CALL" / "PUT"
	Bid             float64 `json:"bid"`
	Ask             float64 `json:"ask"`
	Last            float64 `json:"last"`
	TotalVolume     int64   `json:"totalVolume"`
	OpenInterest    int64   `json:"openInterest"`
	Volatility      float64 `json:"volatility"`  // IV as decimal (0–100 or 0–1, varies)
	ExpirationDate  string  `json:"expirationDate"` // "YYYY-MM-DD"
	DaysToExp       int     `json:"daysToExpiration"`
	StrikePrice     float64 `json:"strikePrice"`
}

// User preferences response (contains streamer info).
type preferencesResponse struct {
	StreamerInfo []struct {
		StreamerSocketURL  string `json:"streamerSocketUrl"`
		CustomerID         string `json:"schwabClientCustomerId"`
		CorrelID           string `json:"schwabClientCorrelId"`
		Channel            string `json:"schwabClientChannel"`
		FunctionID         string `json:"schwabClientFunctionId"`
	} `json:"streamerInfo"`
}

// Quotes response from GET /marketdata/v1/quotes.
// Map key is the active contract symbol (e.g. "/CLM26"); root is in reference.product.
type quotesResponse map[string]quoteEntry

type quoteEntry struct {
	Symbol    string          `json:"symbol"`
	Reference *quoteReference `json:"reference"`
}

type quoteReference struct {
	Product string `json:"product"` // root symbol, e.g. "/CL"
}

// Schwab Streamer WebSocket message types.

type streamerRequest struct {
	Requests []streamerCommand `json:"requests"`
}

type streamerCommand struct {
	Service    string         `json:"service"`
	Command    string         `json:"command"`
	RequestID  int            `json:"requestid"`
	CustomerID string         `json:"SchwabClientCustomerId"`
	CorrelID   string         `json:"SchwabClientCorrelId"`
	Parameters map[string]any `json:"parameters"`
}

type streamerResponse struct {
	Data     []streamerData     `json:"data"`
	Notify   []any              `json:"notify"`
	Response []streamerCmdResp  `json:"response"`
}

type streamerCmdResp struct {
	Service   string `json:"service"`
	Command   string `json:"command"`
	RequestID int    `json:"requestid"`
	Content   struct {
		Code    int    `json:"code"`
		Message string `json:"msg"`
	} `json:"content"`
}

type streamerData struct {
	Service   string                   `json:"service"`
	Timestamp int64                    `json:"timestamp"`
	Command   string                   `json:"command"`
	Content   []map[string]interface{} `json:"content"`
}
