package server

import (
	"encoding/json"
	"errors"
	"fmt"
	log "github.com/Sirupsen/logrus"
	gkmetrics "github.com/go-kit/kit/metrics"
	gkprometheus "github.com/go-kit/kit/metrics/prometheus"
	"github.com/gorilla/mux"
	"github.com/gorilla/schema"
	"github.com/oxfeeefeee/appgo"
	"github.com/oxfeeefeee/appgo/auth"
	"github.com/oxfeeefeee/appgo/toolkit/strutil"
	stdprometheus "github.com/prometheus/client_golang/prometheus"
	"github.com/unrolled/render"
	"net/http"
	"reflect"
	"strings"
	"time"
)

const (
	UserIdFieldName      = "UserId__"
	AdminUserIdFieldName = "AdminUserId__"
	ResIdFieldName       = "ResourceId__"
	ContentFieldName     = "Content__"
	RequestFieldName     = "Request__"
	ConfVerFieldName     = "ConfVer__"

	maxVersion = 99
)

const (
	_ HandlerType = iota
	HandlerTypeJson
	HandlerTypeHtml
)

var decoder = schema.NewDecoder()

var metrics_req_dur gkmetrics.Histogram

var metrics_query_count map[string]gkmetrics.Counter

type HandlerType int

type httpFunc struct {
	requireAuth    bool
	requireAdmin   bool
	hasResId       bool
	hasContent     bool
	hasRequest     bool
	hasConfVer     bool
	dummyInput     bool
	allowAnonymous bool
	inputType      reflect.Type
	contentType    reflect.Type
	funcValue      reflect.Value
}

type handler struct {
	htype    HandlerType
	path     string
	template string
	funcs    map[string]*httpFunc
	supports []string
	ts       TokenStore
	renderer *render.Render
}

func init() {
	decoder.IgnoreUnknownKeys(true)

	if appgo.Conf.Prometheus.Enable {
		metrics_req_dur = gkprometheus.NewSummaryFrom(stdprometheus.SummaryOpts{
			Namespace: "appgo",
			Subsystem: "http",
			Name:      "request_duration_microseconds",
			Help:      "Total time spent serving requests.",
		}, []string{})
		metrics_query_count = map[string]gkmetrics.Counter{
			"all": gkprometheus.NewCounterFrom(stdprometheus.CounterOpts{
				Namespace: "appgo",
				Subsystem: "http",
				Name:      "request_counter",
				Help:      "Total served requests count.",
			}, []string{})}
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer addMetrics(r, time.Now())

	method := r.Method
	ver := apiVersionFromHeader(r)
	if ver > 1 && ver <= maxVersion {
		method += strutil.FromInt(ver)
	}
	f, ok := h.funcs[method]
	if !ok {
		h.renderError(w, appgo.NewApiErr(
			appgo.ECodeNotFound,
			"Bad API version"))
		return
	}
	var input reflect.Value
	if f.dummyInput {
		input = reflect.ValueOf((*appgo.DummyInput)(nil))
	} else {
		input = reflect.New(f.inputType)
		if err := decoder.Decode(input.Interface(), r.URL.Query()); err != nil {
			h.renderError(w, appgo.NewApiErr(appgo.ECodeBadRequest, err.Error()))
			return
		}
	}
	if f.requireAuth {
		user, _ := h.authByHeader(r)
		s := input.Elem()
		field := s.FieldByName(UserIdFieldName)
		if user == 0 {
			if f.allowAnonymous {
				field.SetInt(appgo.AnonymousId)
			} else {
				h.renderError(w, appgo.NewApiErr(
					appgo.ECodeUnauthorized,
					"either remove UserId__ in your input define, or add allowAnonymous tag",
				))
				return
			}
		} else {
			field.SetInt(int64(user))
		}
	} else if f.requireAdmin {
		user, role := h.authByHeader(r)
		s := input.Elem()
		f := s.FieldByName(AdminUserIdFieldName)
		if user == 0 || role != appgo.RoleWebAdmin {
			h.renderError(w, appgo.NewApiErr(
				appgo.ECodeUnauthorized,
				"admin role required, you could remove AdminUserId__ in your input define"))
			return
		}
		f.SetInt(int64(user))
	}
	if f.hasResId {
		vars := mux.Vars(r)
		id := appgo.IdFromStr(vars["id"])
		if id == 0 {
			h.renderError(w, appgo.NewApiErr(
				appgo.ECodeNotFound,
				"ResourceId ('{id}' in url) required, you could remove ResourceId__ in your input define"))
			return
		}
		s := input.Elem()
		f := s.FieldByName(ResIdFieldName)
		f.SetInt(int64(id))
	}
	if f.hasContent {
		content := reflect.New(f.contentType.Elem())
		if err := json.NewDecoder(r.Body).Decode(content.Interface()); err != nil {
			h.renderError(w, appgo.NewApiErr(appgo.ECodeBadRequest, err.Error()))
			return
		}
		s := input.Elem()
		f := s.FieldByName(ContentFieldName)
		f.Set(content)
	}
	if f.hasRequest {
		s := input.Elem()
		f := s.FieldByName(RequestFieldName)
		f.Set(reflect.ValueOf(r))
	}
	if f.hasConfVer {
		ver := confVersionFromHeader(r)
		s := input.Elem()
		f := s.FieldByName(ConfVerFieldName)
		f.Set(reflect.ValueOf(ver))
	}
	argsIn := []reflect.Value{input}
	returns := f.funcValue.Call(argsIn)
	rl := len(returns)
	if !(rl == 1 || rl == 2 || (rl == 3 && h.htype == HandlerTypeHtml)) {
		h.renderError(w, appgo.NewApiErr(appgo.ECodeInternal, "Bad api-func format"))
		return
	}
	// returns (reply, template-name, error) or (reply, error) or returns (error)
	retErr := returns[rl-1]
	// First check if err is nil
	if retErr.IsNil() {
		if rl == 3 {
			template := returns[1].Interface().(string)
			h.renderHtml(w, template, returns[0].Interface())
		} else if rl == 2 {
			h.renderData(w, returns[0].Interface())
		} else { // Empty return
			h.renderData(w, map[string]string{})
		}
	} else {
		if aerr, ok := retErr.Interface().(*appgo.ApiError); !ok {
			aerr = appgo.NewApiErr(appgo.ECodeInternal, "Bad api-func format")
		} else {
			if h.htype == HandlerTypeHtml && aerr.Code == appgo.ECodeRedirect {
				http.Redirect(w, r, aerr.Msg, http.StatusFound)
				return
			}
			h.renderError(w, aerr)
		}
	}
}

func addMetrics(r *http.Request, begin time.Time) {
	if !appgo.Conf.Prometheus.Enable {
		return
	}
	metrics_req_dur.Observe(float64(time.Since(begin) / time.Microsecond))

	path := r.RequestURI
	if i := strings.IndexByte(path, '?'); i > 0 {
		path = path[:i]
	}
	path = strings.Replace(path, "/", "_", -1)
	key := r.Method + path
	if _, ok := metrics_query_count[key]; !ok {
		metrics_query_count[key] = gkprometheus.NewCounterFrom(stdprometheus.CounterOpts{
			Namespace: "appgo",
			Subsystem: "http",
			Name:      "request_counter_" + key,
			Help:      fmt.Sprintf("Total served %s requests count.", key),
		}, []string{})
	}
	metrics_query_count["all"].Add(1)
	metrics_query_count[key].Add(1)

}

func (h *handler) authByHeader(r *http.Request) (appgo.Id, appgo.Role) {
	token := auth.Token(r.Header.Get(appgo.CustomTokenHeaderName))
	user, role := token.Validate()
	if user == 0 {
		return 0, 0
	}
	if !h.ts.Validate(token) {
		return 0, 0
	}
	return user, role
}

func apiVersionFromHeader(r *http.Request) int {
	v := r.Header.Get(appgo.CustomVersionHeaderName)
	return strutil.ToInt(v)
}

func confVersionFromHeader(r *http.Request) int64 {
	v := r.Header.Get(appgo.CustomConfVerHeaderName)
	return strutil.ToInt64(v)
}

func newHandler(funcSet interface{}, htype HandlerType,
	ts TokenStore, renderer *render.Render) *handler {
	funcs := make(map[string]*httpFunc)
	// Let if panic if funSet's type is not right
	path := ""
	template := ""
	t := reflect.TypeOf(funcSet).Elem()
	if field, ok := t.FieldByName("META"); !ok {
		log.Panicln("Bad META setting (path, template)")
	} else {
		if p := field.Tag.Get("path"); p == "" {
			log.Panicln("Empty API path")
		} else {
			path = p
		}
		if htype == HandlerTypeHtml {
			t := field.Tag.Get("template")
			template = t
		}
	}
	structVal := reflect.Indirect(reflect.ValueOf(funcSet))
	supports := make([]string, 0, 4)
	if htype == HandlerTypeJson {
		methods := []string{"GET", "POST", "PUT", "DELETE"}
		for _, m := range methods {
			for i := 1; i <= maxVersion; i++ { //versions
				if i > 1 {
					m += strutil.FromInt(i)
				}
				if fun, err := newHttpFunc(structVal, m); err != nil {
					log.Panicln(err)
				} else if fun != nil {
					funcs[m] = fun
					supports = append(supports, m)
				}
			}
		}
		if len(supports) == 0 {
			log.Panicln("API supports no HTTP method")
		}
	} else if htype == HandlerTypeHtml {
		if fun, err := newHttpFunc(structVal, "HTML"); err != nil {
			log.Panicln(err)
		} else if fun == nil {
			log.Panicln("No HTML function for html")
		} else {
			funcs["GET"] = fun
		}
	} else {
		log.Panicln("Bad handler type")
	}
	return &handler{htype, path, template, funcs, supports, ts, renderer}
}

func newHttpFunc(structVal reflect.Value, fieldName string) (*httpFunc, error) {
	fieldVal := structVal.MethodByName(fieldName)
	if !fieldVal.IsValid() {
		return nil, nil
	}
	ftype := fieldVal.Type()
	inNum := ftype.NumIn()
	if inNum != 1 {
		return nil, errors.New("API func needs to have exact 1 parameter")
	}
	inputType := ftype.In(0)
	dummyInput := false
	if inputType.Kind() != reflect.Ptr {
		return nil, errors.New("API func's parameter needs to be a pointer")
	}
	if inputType == reflect.TypeOf((*appgo.DummyInput)(nil)) {
		dummyInput = true
	}
	inputType = inputType.Elem()
	requireAuth := false
	allowAnonymous := false
	if fromIdField, ok := inputType.FieldByName(UserIdFieldName); ok {
		requireAuth = true
		if fromIdField.Type.Kind() != reflect.Int64 {
			return nil, errors.New("API func's 2nd parameter needs to be Int64")
		}
		aa := fromIdField.Tag.Get("allowAnonymous")
		allowAnonymous = (aa == "true")
	}
	requireAdmin := false
	if fromIdType, ok := inputType.FieldByName(AdminUserIdFieldName); ok {
		requireAdmin = true
		if fromIdType.Type.Kind() != reflect.Int64 {
			return nil, errors.New("API func's 2nd parameter needs to be Int64")
		}
	}
	hasResId := false
	if resIdType, ok := inputType.FieldByName(ResIdFieldName); ok {
		hasResId = true
		if resIdType.Type.Kind() != reflect.Int64 {
			return nil, errors.New("ResId needs to be Int64")
		}
	}
	hasContent := false
	var contentType reflect.Type
	if ctype, ok := inputType.FieldByName(ContentFieldName); ok {
		hasContent = true
		contentType = ctype.Type
		if ctype.Type.Kind() != reflect.Ptr {
			return nil, errors.New("Content needs to be a pointer")
		}
	}
	hasRequest := false
	if ctype, ok := inputType.FieldByName(RequestFieldName); ok {
		hasRequest = true
		if ctype.Type.Kind() != reflect.Ptr {
			return nil, errors.New("Request needs to be a pointer to http.Request")
		}
		if ctype.Type.Elem() != reflect.TypeOf((*http.Request)(nil)).Elem() {
			return nil, errors.New("Request needs to be a pointer to http.Request")
		}
	}
	hasConfVer := false
	if confVerType, ok := inputType.FieldByName(ConfVerFieldName); ok {
		hasConfVer = true
		if confVerType.Type.Kind() != reflect.Int64 {
			return nil, errors.New("ConfVer needs to be Int64")
		}
	}
	return &httpFunc{requireAuth, requireAdmin,
		hasResId, hasContent, hasRequest, hasConfVer,
		dummyInput, allowAnonymous, inputType, contentType, fieldVal}, nil
}
