package s3api

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	"storage/internal/auth"
	"storage/internal/store"
)

func (s *Server) routeCORS(w http.ResponseWriter, r *http.Request, ar *auth.Result, bucket string) {
	switch r.Method {
	case http.MethodGet:
		s.getCORS(w, r, bucket)
	case http.MethodPut:
		s.putCORS(w, r, ar, bucket)
	case http.MethodDelete:
		s.deleteCORS(w, r, bucket)
	default:
		s.err(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed.", r.URL.Path)
	}
}

func (s *Server) getCORS(w http.ResponseWriter, r *http.Request, bucket string) {
	rules, set, err := s.Store.CORS(bucket)
	if err != nil {
		s.storeErr(w, r, err, false)
		return
	}
	if !set {
		s.err(w, r, http.StatusNotFound, "NoSuchCORSConfiguration", "The CORS configuration does not exist.", r.URL.Path)
		return
	}
	writeXML(w, http.StatusOK, corsToXML(rules))
}

func (s *Server) putCORS(w http.ResponseWriter, r *http.Request, ar *auth.Result, bucket string) {
	body, err := s.readLimited(r, ar, 64<<10)
	if err != nil {
		s.storeErr(w, r, err, false)
		return
	}
	var in corsXML
	if err := xml.Unmarshal(body, &in); err != nil || len(in.Rules) == 0 {
		s.err(w, r, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed.", r.URL.Path)
		return
	}
	rules := make([]store.CORSRule, 0, len(in.Rules))
	for _, rule := range in.Rules {
		if len(rule.AllowedOrigin) == 0 || len(rule.AllowedMethod) == 0 {
			s.err(w, r, http.StatusBadRequest, "MalformedXML", "A CORS rule needs an origin and a method.", r.URL.Path)
			return
		}
		rules = append(rules, store.CORSRule{
			Origins: rule.AllowedOrigin,
			Methods: rule.AllowedMethod,
			Headers: rule.AllowedHeader,
			Expose:  rule.ExposeHeader,
			MaxAge:  rule.MaxAgeSeconds,
		})
	}
	if err := s.Store.PutCORS(bucket, rules); err != nil {
		s.storeErr(w, r, err, false)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) deleteCORS(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := s.Store.DeleteCORS(bucket); err != nil {
		s.storeErr(w, r, err, false)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeCORS(w http.ResponseWriter, r *http.Request, bucket string, preflight bool) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	method := r.Method
	if preflight {
		method = r.Header.Get("Access-Control-Request-Method")
		if method == "" {
			return false
		}
	}
	rule, ok := matchCORS(s.corsRules(bucket), origin, method, r.Header.Get("Access-Control-Request-Headers"))
	if !ok {
		return false
	}
	if oneStar(rule.Origins) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	} else {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Allow-Methods", strings.Join(rule.Methods, ", "))
	allow := strings.Join(rule.Headers, ", ")
	if allow == "" || allow == "*" {
		if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
			allow = req
		} else if allow == "" {
			allow = "*"
		}
	}
	w.Header().Set("Access-Control-Allow-Headers", allow)
	if len(rule.Expose) > 0 {
		w.Header().Set("Access-Control-Expose-Headers", strings.Join(rule.Expose, ", "))
	}
	if rule.MaxAge > 0 {
		w.Header().Set("Access-Control-Max-Age", strconv.Itoa(rule.MaxAge))
	}
	return true
}

func (s *Server) corsRules(bucket string) []store.CORSRule {
	if bucket == "" {
		return defaultCORS()
	}
	rules, set, err := s.Store.CORS(bucket)
	if err != nil || !set {
		return defaultCORS()
	}
	return rules
}

func defaultCORS() []store.CORSRule {
	return []store.CORSRule{{
		Origins: []string{"*"},
		Methods: []string{"GET", "PUT", "POST", "DELETE", "HEAD"},
		Headers: []string{"*"},
		Expose:  []string{"ETag", "Content-Length", "Content-Type", "Accept-Ranges", "x-amz-request-id"},
		MaxAge:  3000,
	}}
}

func matchCORS(rules []store.CORSRule, origin, method, requestHeaders string) (store.CORSRule, bool) {
	for _, rule := range rules {
		if !containsOrStar(rule.Origins, origin) || !containsFold(rule.Methods, method) {
			continue
		}
		if requestHeaders != "" && !headersAllowed(rule.Headers, requestHeaders) {
			continue
		}
		return rule, true
	}
	return store.CORSRule{}, false
}

func containsOrStar(list []string, v string) bool {
	for _, item := range list {
		if item == "*" || item == v {
			return true
		}
	}
	return false
}

func containsFold(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(item, v) {
			return true
		}
	}
	return false
}

func headersAllowed(allowed []string, requested string) bool {
	if len(allowed) == 0 || oneStar(allowed) {
		return true
	}
	for _, h := range strings.Split(requested, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if !containsFold(allowed, h) {
			return false
		}
	}
	return true
}

func oneStar(list []string) bool {
	return len(list) == 1 && list[0] == "*"
}

type corsXML struct {
	XMLName xml.Name      `xml:"CORSConfiguration"`
	Xmlns   string        `xml:"xmlns,attr,omitempty"`
	Rules   []corsRuleXML `xml:"CORSRule"`
}

type corsRuleXML struct {
	AllowedOrigin []string `xml:"AllowedOrigin"`
	AllowedMethod []string `xml:"AllowedMethod"`
	AllowedHeader []string `xml:"AllowedHeader,omitempty"`
	ExposeHeader  []string `xml:"ExposeHeader,omitempty"`
	MaxAgeSeconds int      `xml:"MaxAgeSeconds,omitempty"`
}

func corsToXML(rules []store.CORSRule) corsXML {
	out := corsXML{Xmlns: xmlNS}
	for _, rule := range rules {
		out.Rules = append(out.Rules, corsRuleXML{
			AllowedOrigin: rule.Origins,
			AllowedMethod: rule.Methods,
			AllowedHeader: rule.Headers,
			ExposeHeader:  rule.Expose,
			MaxAgeSeconds: rule.MaxAge,
		})
	}
	return out
}
