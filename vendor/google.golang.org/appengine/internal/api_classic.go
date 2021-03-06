// Copyright 2015 Google Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

// +build appengine

package internal

import (
	"errors"
	"net/http"
	"time"

	"appengine"
	"appengine_internal"
	basepb "appengine_internal/base"

	"github.com/golang/protobuf/proto"
	netcontext "golang.org/x/net/context"
)

var contextKey = "holds an appengine.Context"

func fromContext(ctx netcontext.Context) appengine.Context {
	c, _ := ctx.Value(&contextKey).(appengine.Context)
	return c
}

// This is only for classic App Engine adapters.
func ClassicContextFromContext(ctx netcontext.Context) appengine.Context {
	return fromContext(ctx)
}

func withContext(parent netcontext.Context, c appengine.Context) netcontext.Context {
	ctx := netcontext.WithValue(parent, &contextKey, c)

	s := &basepb.StringProto{}
	c.Call("__go__", "GetNamespace", &basepb.VoidProto{}, s, nil)
	if ns := s.GetValue(); ns != "" {
		ctx = WithNamespace(ctx, ns)
	}

	return ctx
}

func IncomingHeaders(ctx netcontext.Context) http.Header {
	if c := fromContext(ctx); c != nil {
		if req, ok := c.Request().(*http.Request); ok {
			return req.Header
		}
	}
	return nil
}

func WithContext(parent netcontext.Context, req *http.Request) netcontext.Context {
	c := appengine.NewContext(req)
	return withContext(parent, c)
}

func Call(ctx netcontext.Context, service, method string, in, out proto.Message) error {
	if f, ctx, ok := callOverrideFromContext(ctx); ok {
		return f(ctx, service, method, in, out)
	}

	c := fromContext(ctx)
	if c == nil {
		// Give a good error message rather than a panic lower down.
		return errors.New("not an App Engine context")
	}

	// Apply transaction modifications if we're in a transaction.
	if t := transactionFromContext(ctx); t != nil {
		if t.finished {
			return errors.New("transaction context has expired")
		}
		applyTransaction(in, &t.transaction)
	}

	var opts *appengine_internal.CallOptions
	if d, ok := ctx.Deadline(); ok {
		opts = &appengine_internal.CallOptions{
			Timeout: d.Sub(time.Now()),
		}
	}

	err := c.Call(service, method, in, out, opts)
	switch v := err.(type) {
	case *appengine_internal.APIError:
		return &APIError{
			Service: v.Service,
			Detail:  v.Detail,
			Code:    v.Code,
		}
	case *appengine_internal.CallError:
		return &CallError{
			Detail:  v.Detail,
			Code:    v.Code,
			Timeout: v.Timeout,
		}
	}
	return err
}

func handleHTTP(w http.ResponseWriter, r *http.Request) {
	panic("handleHTTP called; this should be impossible")
}

func logf(c appengine.Context, level int64, format string, args ...interface{}) {
	var fn func(format string, args ...interface{})
	switch level {
	case 0:
		fn = c.Debugf
	case 1:
		fn = c.Infof
	case 2:
		fn = c.Warningf
	case 3:
		fn = c.Errorf
	case 4:
		fn = c.Criticalf
	default:
		// This shouldn't happen.
		fn = c.Criticalf
	}
	fn(format, args...)
}
