package server

import (
	"blockbook/api"
	"blockbook/bchain"
	"blockbook/common"
	"blockbook/db"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"math/big"
	"net/http"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
)

const txsOnPage = 25
const blocksOnPage = 50
const txsInAPI = 1000

const (
	_ = iota
	apiV1
	apiV2
)

// PublicServer is a handle to public http server
type PublicServer struct {
	binding          string
	certFiles        string
	socketio         *SocketIoServer
	websocket        *WebsocketServer
	https            *http.Server
	db               *db.RocksDB
	txCache          *db.TxCache
	chain            bchain.BlockChain
	chainParser      bchain.BlockChainParser
	api              *api.Worker
	explorerURL      string
	internalExplorer bool
	metrics          *common.Metrics
	is               *common.InternalState
	templates        []*template.Template
	debug            bool
}

// NewPublicServer creates new public server http interface to blockbook and returns its handle
// only basic functionality is mapped, to map all functions, call
func NewPublicServer(binding string, certFiles string, db *db.RocksDB, chain bchain.BlockChain, txCache *db.TxCache, explorerURL string, metrics *common.Metrics, is *common.InternalState, debugMode bool) (*PublicServer, error) {

	api, err := api.NewWorker(db, chain, txCache, is)
	if err != nil {
		return nil, err
	}

	socketio, err := NewSocketIoServer(db, chain, txCache, metrics, is)
	if err != nil {
		return nil, err
	}

	websocket, err := NewWebsocketServer(db, chain, txCache, metrics, is)
	if err != nil {
		return nil, err
	}

	addr, path := splitBinding(binding)
	serveMux := http.NewServeMux()
	https := &http.Server{
		Addr:    addr,
		Handler: serveMux,
	}

	s := &PublicServer{
		binding:          binding,
		certFiles:        certFiles,
		https:            https,
		api:              api,
		socketio:         socketio,
		websocket:        websocket,
		db:               db,
		txCache:          txCache,
		chain:            chain,
		chainParser:      chain.GetChainParser(),
		explorerURL:      explorerURL,
		internalExplorer: explorerURL == "",
		metrics:          metrics,
		is:               is,
		debug:            debugMode,
	}
	s.templates = s.parseTemplates()

	// map only basic functions, the rest is enabled by method MapFullPublicInterface
	serveMux.Handle(path+"favicon.ico", http.FileServer(http.Dir("./static/")))
	serveMux.Handle(path+"static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static/"))))
	// default handler
	serveMux.HandleFunc(path, s.htmlTemplateHandler(s.explorerIndex))
	// default API handler
	serveMux.HandleFunc(path+"api/", s.jsonHandler(s.apiIndex, apiV2))

	return s, nil
}

// Run starts the server
func (s *PublicServer) Run() error {
	if s.certFiles == "" {
		glog.Info("public server: starting to listen on http://", s.https.Addr)
		return s.https.ListenAndServe()
	}
	glog.Info("public server starting to listen on https://", s.https.Addr)
	return s.https.ListenAndServeTLS(fmt.Sprint(s.certFiles, ".crt"), fmt.Sprint(s.certFiles, ".key"))
}

// ConnectFullPublicInterface enables complete public functionality
func (s *PublicServer) ConnectFullPublicInterface() {
	serveMux := s.https.Handler.(*http.ServeMux)
	_, path := splitBinding(s.binding)
    // status page
    serveMux.HandleFunc(path+"status", s.htmlTemplateHandler(s.explorerStatus))
	// support for test pages
	serveMux.Handle(path+"test-socketio.html", http.FileServer(http.Dir("./static/")))
	serveMux.Handle(path+"test-websocket.html", http.FileServer(http.Dir("./static/")))
	if s.internalExplorer {
		// internal explorer handlers
		serveMux.HandleFunc(path+"tx/", s.htmlTemplateHandler(s.explorerTx))
		serveMux.HandleFunc(path+"address/", s.htmlTemplateHandler(s.explorerAddress))
		serveMux.HandleFunc(path+"search/", s.htmlTemplateHandler(s.explorerSearch))
		serveMux.HandleFunc(path+"blocks", s.htmlTemplateHandler(s.explorerBlocks))
		serveMux.HandleFunc(path+"block/", s.htmlTemplateHandler(s.explorerBlock))
		serveMux.HandleFunc(path+"spending/", s.htmlTemplateHandler(s.explorerSpendingTx))
		serveMux.HandleFunc(path+"sendtx", s.htmlTemplateHandler(s.explorerSendTx))
        serveMux.HandleFunc(path+"charts", s.htmlTemplateHandler(s.explorerCharts))
	} else {
		// redirect to wallet requests for tx and address, possibly to external site
		serveMux.HandleFunc(path+"tx/", s.txRedirect)
		serveMux.HandleFunc(path+"address/", s.addressRedirect)
	}
	// API calls
	// default api without version can be changed to different version at any time
	// use versioned api for stability

	var apiDefault int
	// ethereum supports only api V2
	if s.chainParser.GetChainType() == bchain.ChainEthereumType {
		apiDefault = apiV2
	} else {
		apiDefault = apiV1
		// legacy v1 format
		serveMux.HandleFunc(path+"api/v1/block-index/", s.jsonHandler(s.apiBlockIndex, apiV1))
		serveMux.HandleFunc(path+"api/v1/tx-specific/", s.jsonHandler(s.apiTxSpecific, apiV1))
		serveMux.HandleFunc(path+"api/v1/tx/", s.jsonHandler(s.apiTx, apiV1))
		serveMux.HandleFunc(path+"api/v1/address/", s.jsonHandler(s.apiAddress, apiV1))
		serveMux.HandleFunc(path+"api/v1/utxo/", s.jsonHandler(s.apiAddressUtxo, apiV1))
		serveMux.HandleFunc(path+"api/v1/block/", s.jsonHandler(s.apiBlock, apiV1))
		serveMux.HandleFunc(path+"api/v1/sendtx/", s.jsonHandler(s.apiSendTx, apiV1))
		serveMux.HandleFunc(path+"api/v1/estimatefee/", s.jsonHandler(s.apiEstimateFee, apiV1))
        serveMux.HandleFunc(path+"api/v1/findzcserial/", s.jsonHandler(s.apiFindzcserial, apiV1))
	}
	serveMux.HandleFunc(path+"api/block-index/", s.jsonHandler(s.apiBlockIndex, apiDefault))
	serveMux.HandleFunc(path+"api/tx-specific/", s.jsonHandler(s.apiTxSpecific, apiDefault))
	serveMux.HandleFunc(path+"api/tx/", s.jsonHandler(s.apiTx, apiDefault))
	serveMux.HandleFunc(path+"api/address/", s.jsonHandler(s.apiAddress, apiDefault))
	serveMux.HandleFunc(path+"api/utxo/", s.jsonHandler(s.apiAddressUtxo, apiDefault))
	serveMux.HandleFunc(path+"api/block/", s.jsonHandler(s.apiBlock, apiDefault))
	serveMux.HandleFunc(path+"api/sendtx/", s.jsonHandler(s.apiSendTx, apiDefault))
	serveMux.HandleFunc(path+"api/estimatefee/", s.jsonHandler(s.apiEstimateFee, apiDefault))
    serveMux.HandleFunc(path+"api/findzcserial/", s.jsonHandler(s.apiFindzcserial, apiDefault))
	// v2 format
	serveMux.HandleFunc(path+"api/v2/block-index/", s.jsonHandler(s.apiBlockIndex, apiV2))
	serveMux.HandleFunc(path+"api/v2/tx-specific/", s.jsonHandler(s.apiTxSpecific, apiV2))
	serveMux.HandleFunc(path+"api/v2/tx/", s.jsonHandler(s.apiTx, apiV2))
	serveMux.HandleFunc(path+"api/v2/address/", s.jsonHandler(s.apiAddress, apiV2))
	serveMux.HandleFunc(path+"api/v2/utxo/", s.jsonHandler(s.apiAddressUtxo, apiV2))
	serveMux.HandleFunc(path+"api/v2/block/", s.jsonHandler(s.apiBlock, apiV2))
	serveMux.HandleFunc(path+"api/v2/sendtx/", s.jsonHandler(s.apiSendTx, apiV2))
	serveMux.HandleFunc(path+"api/v2/estimatefee/", s.jsonHandler(s.apiEstimateFee, apiV2))
    serveMux.HandleFunc(path+"api/v2/findzcserial/", s.jsonHandler(s.apiFindzcserial, apiV2))
	// socket.io interface
	serveMux.Handle(path+"socket.io/", s.socketio.GetHandler())
	// websocket interface
	serveMux.Handle(path+"websocket", s.websocket.GetHandler())
}

// Close closes the server
func (s *PublicServer) Close() error {
	glog.Infof("public server: closing")
	return s.https.Close()
}

// Shutdown shuts down the server
func (s *PublicServer) Shutdown(ctx context.Context) error {
	glog.Infof("public server: shutdown")
	return s.https.Shutdown(ctx)
}

// OnNewBlock notifies users subscribed to bitcoind/hashblock about new block
func (s *PublicServer) OnNewBlock(hash string, height uint32) {
	s.socketio.OnNewBlockHash(hash)
	s.websocket.OnNewBlock(hash, height)
}

// OnNewTxAddr notifies users subscribed to bitcoind/addresstxid about new block
func (s *PublicServer) OnNewTxAddr(tx *bchain.Tx, desc bchain.AddressDescriptor) {
	s.socketio.OnNewTxAddr(tx.Txid, desc)
	s.websocket.OnNewTxAddr(tx, desc)
}

func (s *PublicServer) txRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, joinURL(s.explorerURL, r.URL.Path), 302)
	s.metrics.ExplorerViews.With(common.Labels{"action": "tx-redirect"}).Inc()
}

func (s *PublicServer) addressRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, joinURL(s.explorerURL, r.URL.Path), 302)
	s.metrics.ExplorerViews.With(common.Labels{"action": "address-redirect"}).Inc()
}

func splitBinding(binding string) (addr string, path string) {
	i := strings.Index(binding, "/")
	if i >= 0 {
		return binding[0:i], binding[i:]
	}
	return binding, "/"
}

func joinURL(base string, part string) string {
	if len(base) > 0 {
		if len(base) > 0 && base[len(base)-1] == '/' && len(part) > 0 && part[0] == '/' {
			return base + part[1:]
		}
		return base + part
	}
	return part
}

func getFunctionName(i interface{}) string {
	return runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
}

func (s *PublicServer) jsonHandler(handler func(r *http.Request, apiVersion int) (interface{}, error), apiVersion int) func(w http.ResponseWriter, r *http.Request) {
	type jsonError struct {
		Text       string `json:"error"`
		HTTPStatus int    `json:"-"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var data interface{}
		var err error
		defer func() {
			if e := recover(); e != nil {
				glog.Error(getFunctionName(handler), " recovered from panic: ", e)
				debug.PrintStack()
				if s.debug {
					data = jsonError{fmt.Sprint("Internal server error: recovered from panic ", e), http.StatusInternalServerError}
				} else {
					data = jsonError{"Internal server error", http.StatusInternalServerError}
				}
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			if e, isError := data.(jsonError); isError {
				w.WriteHeader(e.HTTPStatus)
			}
			json.NewEncoder(w).Encode(data)
		}()
		data, err = handler(r, apiVersion)
		if err != nil || data == nil {
			if apiErr, ok := err.(*api.APIError); ok {
				if apiErr.Public {
					data = jsonError{apiErr.Error(), http.StatusBadRequest}
				} else {
					data = jsonError{apiErr.Error(), http.StatusInternalServerError}
				}
			} else {
				if err != nil {
					glog.Error(getFunctionName(handler), " error: ", err)
				}
				if s.debug {
					if data != nil {
						data = jsonError{fmt.Sprintf("Internal server error: %v, data %+v", err, data), http.StatusInternalServerError}
					} else {
						data = jsonError{fmt.Sprintf("Internal server error: %v", err), http.StatusInternalServerError}
					}
				} else {
					data = jsonError{"Internal server error", http.StatusInternalServerError}
				}
			}
		}
	}
}

func (s *PublicServer) newTemplateData() *TemplateData {
	return &TemplateData{
		CoinName:         s.is.Coin,
		CoinShortcut:     s.is.CoinShortcut,
		CoinLabel:        s.is.CoinLabel,
		ChainType:        s.chainParser.GetChainType(),
		InternalExplorer: s.internalExplorer && !s.is.InitialSync,
		TOSLink:          api.Text.TOSLink,
        IsIndex:          false,
	}
}

func (s *PublicServer) newTemplateDataWithError(text string) *TemplateData {
	td := s.newTemplateData()
	td.Error = &api.APIError{Text: text}
	return td
}

func (s *PublicServer) htmlTemplateHandler(handler func(w http.ResponseWriter, r *http.Request) (tpl, *TemplateData, error)) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var t tpl
		var data *TemplateData
		var err error
		defer func() {
			if e := recover(); e != nil {
				glog.Error(getFunctionName(handler), " recovered from panic: ", e)
				debug.PrintStack()
				t = errorInternalTpl
				if s.debug {
					data = s.newTemplateDataWithError(fmt.Sprint("Internal server error: recovered from panic ", e))
				} else {
					data = s.newTemplateDataWithError("Internal server error")
				}
			}
			// noTpl means the handler completely handled the request
			if t != noTpl {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				// return 500 Internal Server Error with errorInternalTpl
				if t == errorInternalTpl {
					w.WriteHeader(http.StatusInternalServerError)
				}
				if err := s.templates[t].ExecuteTemplate(w, "base.html", data); err != nil {
					glog.Error(err)
				}
			}
		}()
		if s.debug {
			// reload templates on each request
			// to reflect changes during development
			s.templates = s.parseTemplates()
		}
		t, data, err = handler(w, r)
		if err != nil || (data == nil && t != noTpl) {
			t = errorInternalTpl
			if apiErr, ok := err.(*api.APIError); ok {
				data = s.newTemplateData()
				data.Error = apiErr
				if apiErr.Public {
					t = errorTpl
				}
			} else {
				if err != nil {
					glog.Error(getFunctionName(handler), " error: ", err)
				}
				if s.debug {
					data = s.newTemplateDataWithError(fmt.Sprintf("Internal server error: %v, data %+v", err, data))
				} else {
					data = s.newTemplateDataWithError("Internal server error")
				}
			}
		}
	}
}

type tpl int

const (
	noTpl = tpl(iota)
	errorTpl
	errorInternalTpl
	indexTpl
    statusTpl
	txTpl
	addressTpl
	blocksTpl
	blockTpl
	sendTransactionTpl
    chartsTpl

	tplCount
)

// TemplateData is used to transfer data to the templates
type TemplateData struct {
    IsIndex          bool
	CoinName         string
	CoinShortcut     string
	CoinLabel        string
	InternalExplorer bool
	ChainType        bchain.ChainType
	Address          *api.Address
	AddrStr          string
	Tx               *api.Tx
	Error            *api.APIError
	Blocks           *api.Blocks
	Block            *api.Block
	Info             *api.SystemInfo
	Page             int
	PrevPage         int
	NextPage         int
	PagingRange      []int
	PageParams       template.URL
	TOSLink          string
	SendTxHex        string
	Status           string
    ChartData        string
}

func (s *PublicServer) parseTemplates() []*template.Template {
	templateFuncMap := template.FuncMap{
		"formatTime":               formatTime,
		"formatUnixTime":           formatUnixTime,
		"formatAmount":             s.formatAmount,
		"formatAmountWithDecimals": formatAmountWithDecimals,
		"setTxToTemplateData":      setTxToTemplateData,
		"stringInSlice":            stringInSlice,
        "formatAmount2":            formatAmount2,
        "getPercent":               getPercent,
        "formatAmount0":            formatAmount0,
        "formatDenom":              formatDenom,
	}
	var createTemplate func(filenames ...string) *template.Template
	if s.debug {
		createTemplate = func(filenames ...string) *template.Template {
			if len(filenames) == 0 {
				panic("Missing templates")
			}
			return template.Must(template.New(filepath.Base(filenames[0])).Funcs(templateFuncMap).ParseFiles(filenames...))
		}
	} else {
		createTemplate = func(filenames ...string) *template.Template {
			if len(filenames) == 0 {
				panic("Missing templates")
			}
			t := template.New(filepath.Base(filenames[0])).Funcs(templateFuncMap)
			for _, filename := range filenames {
				b, err := ioutil.ReadFile(filename)
				if err != nil {
					panic(err)
				}
				// perform very simple minification - replace leading spaces used as formatting and new lines
				r := regexp.MustCompile(`\n\s*`)
				b = r.ReplaceAll(b, []byte{})
				s := string(b)
				name := filepath.Base(filename)
				var tt *template.Template
				if name == t.Name() {
					tt = t
				} else {
					tt = t.New(name)
				}
				_, err = tt.Parse(s)
				if err != nil {
					panic(err)
				}
			}
			return t
		}
	}
	t := make([]*template.Template, tplCount)
	t[errorTpl] = createTemplate("./static/templates/error.html", "./static/templates/base.html")
	t[errorInternalTpl] = createTemplate("./static/templates/error.html", "./static/templates/base.html")
	t[indexTpl] = createTemplate("./static/templates/index.html", "./static/templates/base.html")
    t[statusTpl] = createTemplate("./static/templates/status.html", "./static/templates/base.html")
	t[blocksTpl] = createTemplate("./static/templates/blocks.html", "./static/templates/paging.html", "./static/templates/base.html")
	t[sendTransactionTpl] = createTemplate("./static/templates/sendtx.html", "./static/templates/base.html")
    t[chartsTpl] = createTemplate("./static/templates/charts.html", "./static/templates/base.html")
	if s.chainParser.GetChainType() == bchain.ChainEthereumType {
		t[txTpl] = createTemplate("./static/templates/tx.html", "./static/templates/txdetail_ethereumtype.html", "./static/templates/base.html")
		t[addressTpl] = createTemplate("./static/templates/address.html", "./static/templates/txdetail_ethereumtype.html", "./static/templates/paging.html", "./static/templates/base.html")
		t[blockTpl] = createTemplate("./static/templates/block.html", "./static/templates/txdetail_ethereumtype.html", "./static/templates/paging.html", "./static/templates/base.html")
	} else {
		t[txTpl] = createTemplate("./static/templates/tx.html", "./static/templates/txdetail.html", "./static/templates/base.html")
		t[addressTpl] = createTemplate("./static/templates/address.html", "./static/templates/txdetail.html", "./static/templates/paging.html", "./static/templates/base.html")
		t[blockTpl] = createTemplate("./static/templates/block.html", "./static/templates/txdetail.html", "./static/templates/paging.html", "./static/templates/base.html")
	}
	return t
}

func formatUnixTime(ut int64) string {
	return formatTime(time.Unix(ut, 0))
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC1123)
}

// for now return the string as it is
// in future could be used to do coin specific formatting
func (s *PublicServer) formatAmount(a *api.Amount) string {
	return s.chainParser.AmountToDecimalString((*big.Int)(a))
}

func formatAmountWithDecimals(a *api.Amount, d int) string {
	if a == nil {
		return "0"
	}
	return a.DecimalString(d)
}

// called from template to support txdetail.html functionality
func setTxToTemplateData(td *TemplateData, tx *api.Tx) *TemplateData {
	td.Tx = tx
	return td
}

func (s *PublicServer) explorerTx(w http.ResponseWriter, r *http.Request) (tpl, *TemplateData, error) {
	var tx *api.Tx
	var err error
	s.metrics.ExplorerViews.With(common.Labels{"action": "tx"}).Inc()
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		txid := r.URL.Path[i+1:]
		tx, err = s.api.GetTransaction(txid, false, true)
		if err != nil {
			return errorTpl, nil, err
		}
	}
	data := s.newTemplateData()
	data.Tx = tx
	return txTpl, data, nil
}

func (s *PublicServer) explorerSpendingTx(w http.ResponseWriter, r *http.Request) (tpl, *TemplateData, error) {
	s.metrics.ExplorerViews.With(common.Labels{"action": "spendingtx"}).Inc()
	var err error
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) > 2 {
		tx := parts[len(parts)-2]
		n, ec := strconv.Atoi(parts[len(parts)-1])
		if ec == nil {
			spendingTx, err := s.api.GetSpendingTxid(tx, n)
			if err == nil && spendingTx != "" {
				http.Redirect(w, r, joinURL("/tx/", spendingTx), 302)
				return noTpl, nil, nil
			}
		}
	}
	if err == nil {
		err = api.NewAPIError("Transaction not found", true)
	}
	return errorTpl, nil, err
}

func (s *PublicServer) explorerAddress(w http.ResponseWriter, r *http.Request) (tpl, *TemplateData, error) {
	var address *api.Address
	var filter string
	var fn = api.AddressFilterVoutOff
	var err error
	s.metrics.ExplorerViews.With(common.Labels{"action": "address"}).Inc()
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		page, ec := strconv.Atoi(r.URL.Query().Get("page"))
		if ec != nil {
			page = 0
		}
		filter = r.URL.Query().Get("filter")
		if len(filter) > 0 {
			if filter == "inputs" {
				fn = api.AddressFilterVoutInputs
			} else if filter == "outputs" {
				fn = api.AddressFilterVoutOutputs
			} else {
				fn, ec = strconv.Atoi(filter)
				if ec != nil || fn < 0 {
					filter = ""
					fn = api.AddressFilterVoutOff
				}
			}
		}
		address, err = s.api.GetAddress(r.URL.Path[i+1:], page, txsOnPage, api.TxHistoryLight, &api.AddressFilter{Vout: fn})
		if err != nil {
			return errorTpl, nil, err
		}
	}
	data := s.newTemplateData()
	data.AddrStr = address.AddrStr
	data.Address = address
	data.Page = address.Page
	data.PagingRange, data.PrevPage, data.NextPage = getPagingRange(address.Page, address.TotalPages)
	if filter != "" {
		data.PageParams = template.URL("&filter=" + filter)
		data.Address.Filter = filter
	}
	return addressTpl, data, nil
}

func (s *PublicServer) explorerBlocks(w http.ResponseWriter, r *http.Request) (tpl, *TemplateData, error) {
	var blocks *api.Blocks
	var err error
	s.metrics.ExplorerViews.With(common.Labels{"action": "blocks"}).Inc()
	page, ec := strconv.Atoi(r.URL.Query().Get("page"))
	if ec != nil {
		page = 0
	}
	blocks, err = s.api.GetBlocks(page, blocksOnPage)
	if err != nil {
		return errorTpl, nil, err
	}
	data := s.newTemplateData()
	data.Blocks = blocks
	data.Page = blocks.Page
	data.PagingRange, data.PrevPage, data.NextPage = getPagingRange(blocks.Page, blocks.TotalPages)
	return blocksTpl, data, nil
}

func (s *PublicServer) explorerBlock(w http.ResponseWriter, r *http.Request) (tpl, *TemplateData, error) {
	var block *api.Block
	var err error
	s.metrics.ExplorerViews.With(common.Labels{"action": "block"}).Inc()
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		page, ec := strconv.Atoi(r.URL.Query().Get("page"))
		if ec != nil {
			page = 0
		}
		block, err = s.api.GetBlock(r.URL.Path[i+1:], page, txsOnPage)
		if err != nil {
			return errorTpl, nil, err
		}
	}
	data := s.newTemplateData()
	data.Block = block
	data.Page = block.Page
	data.PagingRange, data.PrevPage, data.NextPage = getPagingRange(block.Page, block.TotalPages)
	return blockTpl, data, nil
}

func (s *PublicServer) explorerIndex(w http.ResponseWriter, r *http.Request) (tpl, *TemplateData, error) {
	var si *api.SystemInfo
    var blocks *api.Blocks
	var err error
	s.metrics.ExplorerViews.With(common.Labels{"action": "index"}).Inc()
	si, err = s.api.GetSystemInfo(false)
	if err != nil {
		return errorTpl, nil, err
	}
    // get just five blocks
    blocks, err = s.api.GetBlocks(0, 5)
    if err != nil {
        return errorTpl, nil, err
    }
	data := s.newTemplateData()
    data.IsIndex = true
	data.Info = si
    data.Blocks = blocks
	return indexTpl, data, nil
}


func (s *PublicServer) explorerStatus(w http.ResponseWriter, r *http.Request) (tpl, *TemplateData, error) {
	var si *api.SystemInfo
	var err error
	s.metrics.ExplorerViews.With(common.Labels{"action": "status"}).Inc()
	si, err = s.api.GetSystemInfo(false)
	if err != nil {
		return errorTpl, nil, err
	}
	data := s.newTemplateData()
	data.Info = si
	return statusTpl, data, nil
}

func (s *PublicServer) explorerSearch(w http.ResponseWriter, r *http.Request) (tpl, *TemplateData, error) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var tx *api.Tx
	var address *api.Address
	var block *api.Block
	var err error
	s.metrics.ExplorerViews.With(common.Labels{"action": "search"}).Inc()
	if len(q) > 0 {
		block, err = s.api.GetBlock(q, 0, 1)
		if err == nil {
			http.Redirect(w, r, joinURL("/block/", block.Hash), 302)
			return noTpl, nil, nil
		}
		tx, err = s.api.GetTransaction(q, false, false)
		if err == nil {
			http.Redirect(w, r, joinURL("/tx/", tx.Txid), 302)
			return noTpl, nil, nil
		}
		address, err = s.api.GetAddress(q, 0, 1, api.Basic, &api.AddressFilter{Vout: api.AddressFilterVoutOff})
		if err == nil {
			http.Redirect(w, r, joinURL("/address/", address.AddrStr), 302)
			return noTpl, nil, nil
		}
	}
	return errorTpl, nil, api.NewAPIError(fmt.Sprintf("No matching records found for '%v'", q), true)
}

func (s *PublicServer) explorerSendTx(w http.ResponseWriter, r *http.Request) (tpl, *TemplateData, error) {
	s.metrics.ExplorerViews.With(common.Labels{"action": "sendtx"}).Inc()
	data := s.newTemplateData()
	if r.Method == http.MethodPost {
		err := r.ParseForm()
		if err != nil {
			return sendTransactionTpl, data, err
		}
		hex := r.FormValue("hex")
		if len(hex) > 0 {
			res, err := s.chain.SendRawTransaction(hex)
			if err != nil {
				data.SendTxHex = hex
				data.Error = &api.APIError{Text: err.Error(), Public: true}
				return sendTransactionTpl, data, nil
			}
			data.Status = "Transaction sent, result " + res
		}
	}
	return sendTransactionTpl, data, nil
}

func (s *PublicServer) explorerCharts(w http.ResponseWriter, r *http.Request) (tpl, *TemplateData, error) {
	data := s.newTemplateData()
    absPath, _ := filepath.Abs("../plot_data/zznzsupplydata.json")
    jsonFile, err := ioutil.ReadFile(absPath)
    // Load data from json
    if err != nil {
        return errorTpl, nil, err
    }
    data.ChartData = string(jsonFile)
	return chartsTpl, data, nil
}

func getPagingRange(page int, total int) ([]int, int, int) {
	// total==-1 means total is unknown, show only prev/next buttons
	if total >= 0 && total < 2 {
		return nil, 0, 0
	}
	var r []int
	pp, np := page-1, page+1
	if pp < 1 {
		pp = 1
	}
	if total > 0 {
		if np > total {
			np = total
		}
		r = make([]int, 0, 8)
		if total < 6 {
			for i := 1; i <= total; i++ {
				r = append(r, i)
			}
		} else {
			r = append(r, 1)
			if page > 3 {
				r = append(r, 0)
			}
			if pp == 1 {
				if page == 1 {
					r = append(r, np)
					r = append(r, np+1)
					r = append(r, np+2)
				} else {
					r = append(r, page)
					r = append(r, np)
					r = append(r, np+1)
				}
			} else if np == total {
				if page == total {
					r = append(r, pp-2)
					r = append(r, pp-1)
					r = append(r, pp)
				} else {
					r = append(r, pp-1)
					r = append(r, pp)
					r = append(r, page)
				}
			} else {
				r = append(r, pp)
				r = append(r, page)
				r = append(r, np)
			}
			if page <= total-3 {
				r = append(r, 0)
			}
			r = append(r, total)
		}
	}
	return r, pp, np
}

func (s *PublicServer) apiIndex(r *http.Request, apiVersion int) (interface{}, error) {
	s.metrics.ExplorerViews.With(common.Labels{"action": "api-index"}).Inc()
	return s.api.GetSystemInfo(false)
}

func (s *PublicServer) apiBlockIndex(r *http.Request, apiVersion int) (interface{}, error) {
	type resBlockIndex struct {
		BlockHash string `json:"blockHash"`
	}
	var err error
	var hash string
	height := -1
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		if h, err := strconv.Atoi(r.URL.Path[i+1:]); err == nil {
			height = h
		}
	}
	if height >= 0 {
		hash, err = s.db.GetBlockHash(uint32(height))
	} else {
		_, hash, err = s.db.GetBestBlock()
	}
	if err != nil {
		glog.Error(err)
		return nil, err
	}
	return resBlockIndex{
		BlockHash: hash,
	}, nil
}

func (s *PublicServer) apiTx(r *http.Request, apiVersion int) (interface{}, error) {
	var tx *api.Tx
	var err error
	s.metrics.ExplorerViews.With(common.Labels{"action": "api-tx"}).Inc()
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		txid := r.URL.Path[i+1:]
		spendingTxs := false
		p := r.URL.Query().Get("spending")
		if len(p) > 0 {
			spendingTxs, err = strconv.ParseBool(p)
			if err != nil {
				return nil, api.NewAPIError("Parameter 'spending' cannot be converted to boolean", true)
			}
		}
		tx, err = s.api.GetTransaction(txid, spendingTxs, false)
		if err == nil && apiVersion == apiV1 {
			return s.api.TxToV1(tx), nil
		}
	}
	return tx, err
}

func (s *PublicServer) apiTxSpecific(r *http.Request, apiVersion int) (interface{}, error) {
	var tx json.RawMessage
	var err error
	s.metrics.ExplorerViews.With(common.Labels{"action": "api-tx-specific"}).Inc()
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		txid := r.URL.Path[i+1:]
		tx, err = s.chain.GetTransactionSpecific(&bchain.Tx{Txid: txid})
	}
	return tx, err
}

func (s *PublicServer) apiAddress(r *http.Request, apiVersion int) (interface{}, error) {
	var address *api.Address
	var err error
	s.metrics.ExplorerViews.With(common.Labels{"action": "api-address"}).Inc()
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		page, ec := strconv.Atoi(r.URL.Query().Get("page"))
		if ec != nil {
			page = 0
		}
		address, err = s.api.GetAddress(r.URL.Path[i+1:], page, txsInAPI, api.TxidHistory, &api.AddressFilter{Vout: api.AddressFilterVoutOff})
		if err == nil && apiVersion == apiV1 {
			return s.api.AddressToV1(address), nil
		}
	}
	return address, err
}

func (s *PublicServer) apiAddressUtxo(r *http.Request, apiVersion int) (interface{}, error) {
	var utxo []api.AddressUtxo
	var err error
	s.metrics.ExplorerViews.With(common.Labels{"action": "api-address"}).Inc()
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		onlyConfirmed := false
		c := r.URL.Query().Get("confirmed")
		if len(c) > 0 {
			onlyConfirmed, err = strconv.ParseBool(c)
			if err != nil {
				return nil, api.NewAPIError("Parameter 'confirmed' cannot be converted to boolean", true)
			}
		}
		utxo, err = s.api.GetAddressUtxo(r.URL.Path[i+1:], onlyConfirmed)
		if err == nil && apiVersion == apiV1 {
			return s.api.AddressUtxoToV1(utxo), nil
		}
	}
	return utxo, err
}

func (s *PublicServer) apiBlock(r *http.Request, apiVersion int) (interface{}, error) {
	var block *api.Block
	var err error
	s.metrics.ExplorerViews.With(common.Labels{"action": "api-block"}).Inc()
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		page, ec := strconv.Atoi(r.URL.Query().Get("page"))
		if ec != nil {
			page = 0
		}
		block, err = s.api.GetBlock(r.URL.Path[i+1:], page, txsInAPI)
		if err == nil && apiVersion == apiV1 {
			return s.api.BlockToV1(block), nil
		}
	}
	return block, err
}

type resultSendTransaction struct {
	Result string `json:"result"`
}

func (s *PublicServer) apiSendTx(r *http.Request, apiVersion int) (interface{}, error) {
	var err error
	var res resultSendTransaction
	var hex string
	s.metrics.ExplorerViews.With(common.Labels{"action": "api-sendtx"}).Inc()
	if r.Method == http.MethodPost {
		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			return nil, api.NewAPIError("Missing tx blob", true)
		}
		hex = string(data)
	} else {
		if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
			hex = r.URL.Path[i+1:]
		}
	}
	if len(hex) > 0 {
		res.Result, err = s.chain.SendRawTransaction(hex)
		if err != nil {
			return nil, api.NewAPIError(err.Error(), true)
		}
		return res, nil
	}
	return nil, api.NewAPIError("Missing tx blob", true)
}

type resultEstimateFeeAsString struct {
	Result string `json:"result"`
}

func (s *PublicServer) apiEstimateFee(r *http.Request, apiVersion int) (interface{}, error) {
	var res resultEstimateFeeAsString
	s.metrics.ExplorerViews.With(common.Labels{"action": "api-estimatefee"}).Inc()
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		b := r.URL.Path[i+1:]
		if len(b) > 0 {
			blocks, err := strconv.Atoi(b)
			if err != nil {
				return nil, api.NewAPIError("Parameter 'number of blocks' is not a number", true)
			}
			conservative := true
			c := r.URL.Query().Get("conservative")
			if len(c) > 0 {
				conservative, err = strconv.ParseBool(c)
				if err != nil {
					return nil, api.NewAPIError("Parameter 'conservative' cannot be converted to boolean", true)
				}
			}
			var fee big.Int
			fee, err = s.chain.EstimateSmartFee(blocks, conservative)
			if err != nil {
				fee, err = s.chain.EstimateFee(blocks)
				if err != nil {
					return nil, err
				}
			}
			res.Result = s.chainParser.AmountToDecimalString(&fee)
			return res, nil
		}
	}
	return nil, api.NewAPIError("Missing parameter 'number of blocks'", true)
}


func (s *PublicServer) apiFindzcserial(r *http.Request, apiVersion int) (interface{}, error) {
    s.metrics.ExplorerViews.With(common.Labels{"action": "api-findzcserial"}).Inc()
    if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
        serialHex := r.URL.Path[i+1:]
        txid, err := s.chain.Findzcserial(serialHex)
        if err != nil {
            return nil, err
        }
        return txid, nil
    }

    return nil, api.NewAPIError("Missing parameter 'serialHex'", true)
}


// formatAmount2 prints float rounding to two decimals
func formatAmount2(a json.Number) string {
    val, _ := a.Float64()
    return fmt.Sprintf("%.8f", val)
}

// formatAmount0 prints float rounding to zero decimals
func formatAmount0(a json.Number) string {
    val, _ := a.Float64()
    return fmt.Sprintf("%.0f", val)
}


// getPercent returns the float to 2 decimal places and appends %
func getPercent(a json.Number, b json.Number) string {
    x, _ := a.Float64()
    y, _ := b.Float64()
    percent := 100 * x / y
    return fmt.Sprintf("%.2f%%", percent)
}


// returns the amount of tokens on a given zerocoin denom
func formatDenom(supply json.Number, den int) string {
    val, _ := supply.Float64()
    coins := val / float64(den)
    return fmt.Sprintf("%.0f", coins)
}
