// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"git.curoverse.com/arvados.git/sdk/go/arvados"
	"git.curoverse.com/arvados.git/sdk/go/auth"
)

var pathPattern = `^/arvados/v1/%s(/([0-9a-z]{5})-%s-[0-9a-z]{15})?(.*)$`
var wfRe = regexp.MustCompile(fmt.Sprintf(pathPattern, "workflows", "7fd4e"))
var containersRe = regexp.MustCompile(fmt.Sprintf(pathPattern, "containers", "dz642"))
var containerRequestsRe = regexp.MustCompile(fmt.Sprintf(pathPattern, "container_requests", "xvhdp"))
var collectionRe = regexp.MustCompile(fmt.Sprintf(pathPattern, "collections", "4zz18"))
var collectionByPDHRe = regexp.MustCompile(`^/arvados/v1/collections/([0-9a-fA-F]{32}\+[0-9]+)+$`)

func (h *Handler) remoteClusterRequest(remoteID string, req *http.Request) (*http.Response, error) {
	remote, ok := h.Cluster.RemoteClusters[remoteID]
	if !ok {
		return nil, HTTPError{fmt.Sprintf("no proxy available for cluster %v", remoteID), http.StatusNotFound}
	}
	scheme := remote.Scheme
	if scheme == "" {
		scheme = "https"
	}
	saltedReq, err := h.saltAuthToken(req, remoteID)
	if err != nil {
		return nil, err
	}
	urlOut := &url.URL{
		Scheme:   scheme,
		Host:     remote.Host,
		Path:     saltedReq.URL.Path,
		RawPath:  saltedReq.URL.RawPath,
		RawQuery: saltedReq.URL.RawQuery,
	}
	client := h.secureClient
	if remote.Insecure {
		client = h.insecureClient
	}
	return h.proxy.ForwardRequest(saltedReq, urlOut, client)
}

// Buffer request body, parse form parameters in request, and then
// replace original body with the buffer so it can be re-read by
// downstream proxy steps.
func loadParamsFromForm(req *http.Request) error {
	var postBody *bytes.Buffer
	if req.Body != nil && req.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		var cl int64
		if req.ContentLength > 0 {
			cl = req.ContentLength
		}
		postBody = bytes.NewBuffer(make([]byte, 0, cl))
		originalBody := req.Body
		defer originalBody.Close()
		req.Body = ioutil.NopCloser(io.TeeReader(req.Body, postBody))
	}

	err := req.ParseForm()
	if err != nil {
		return err
	}

	if req.Body != nil && postBody != nil {
		req.Body = ioutil.NopCloser(postBody)
	}
	return nil
}

func (h *Handler) setupProxyRemoteCluster(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/arvados/v1/workflows", &genericFederatedRequestHandler{next, h, wfRe})
	mux.Handle("/arvados/v1/workflows/", &genericFederatedRequestHandler{next, h, wfRe})
	mux.Handle("/arvados/v1/containers", &genericFederatedRequestHandler{next, h, containersRe})
	mux.Handle("/arvados/v1/containers/", &genericFederatedRequestHandler{next, h, containersRe})
	mux.Handle("/arvados/v1/container_requests", &genericFederatedRequestHandler{next, h, containerRequestsRe})
	mux.Handle("/arvados/v1/container_requests/", &genericFederatedRequestHandler{next, h, containerRequestsRe})
	mux.Handle("/arvados/v1/collections", next)
	mux.Handle("/arvados/v1/collections/", &collectionFederatedRequestHandler{next, h})
	mux.Handle("/", next)

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		parts := strings.Split(req.Header.Get("Authorization"), "/")
		alreadySalted := (len(parts) == 3 && parts[0] == "Bearer v2" && len(parts[2]) == 40)

		if alreadySalted ||
			strings.Index(req.Header.Get("Via"), "arvados-controller") != -1 {
			// The token is already salted, or this is a
			// request from another instance of
			// arvados-controller.  In either case, we
			// don't want to proxy this query, so just
			// continue down the instance handler stack.
			next.ServeHTTP(w, req)
			return
		}

		mux.ServeHTTP(w, req)
	})

	return mux
}

type CurrentUser struct {
	Authorization arvados.APIClientAuthorization
	UUID          string
}

func (h *Handler) validateAPItoken(req *http.Request, user *CurrentUser) error {
	db, err := h.db(req)
	if err != nil {
		return err
	}
	return db.QueryRowContext(req.Context(), `SELECT api_client_authorizations.uuid, users.uuid FROM api_client_authorizations JOIN users on api_client_authorizations.user_id=users.id WHERE api_token=$1 AND (expires_at IS NULL OR expires_at > current_timestamp) LIMIT 1`, user.Authorization.APIToken).Scan(&user.Authorization.UUID, &user.UUID)
}

// Extract the auth token supplied in req, and replace it with a
// salted token for the remote cluster.
func (h *Handler) saltAuthToken(req *http.Request, remote string) (updatedReq *http.Request, err error) {
	updatedReq = (&http.Request{
		Method:        req.Method,
		URL:           req.URL,
		Header:        req.Header,
		Body:          req.Body,
		ContentLength: req.ContentLength,
		Host:          req.Host,
	}).WithContext(req.Context())

	creds := auth.NewCredentials()
	creds.LoadTokensFromHTTPRequest(updatedReq)
	if len(creds.Tokens) == 0 && updatedReq.Header.Get("Content-Type") == "application/x-www-form-encoded" {
		// Override ParseForm's 10MiB limit by ensuring
		// req.Body is a *http.maxBytesReader.
		updatedReq.Body = http.MaxBytesReader(nil, updatedReq.Body, 1<<28) // 256MiB. TODO: use MaxRequestSize from discovery doc or config.
		if err := creds.LoadTokensFromHTTPRequestBody(updatedReq); err != nil {
			return nil, err
		}
		// Replace req.Body with a buffer that re-encodes the
		// form without api_token, in case we end up
		// forwarding the request.
		if updatedReq.PostForm != nil {
			updatedReq.PostForm.Del("api_token")
		}
		updatedReq.Body = ioutil.NopCloser(bytes.NewBufferString(updatedReq.PostForm.Encode()))
	}
	if len(creds.Tokens) == 0 {
		return updatedReq, nil
	}

	token, err := auth.SaltToken(creds.Tokens[0], remote)

	if err == auth.ErrObsoleteToken {
		// If the token exists in our own database, salt it
		// for the remote. Otherwise, assume it was issued by
		// the remote, and pass it through unmodified.
		currentUser := CurrentUser{Authorization: arvados.APIClientAuthorization{APIToken: creds.Tokens[0]}}
		err = h.validateAPItoken(req, &currentUser)
		if err == sql.ErrNoRows {
			// Not ours; pass through unmodified.
			token = currentUser.Authorization.APIToken
		} else if err != nil {
			return nil, err
		} else {
			// Found; make V2 version and salt it.
			token, err = auth.SaltToken(currentUser.Authorization.TokenV2(), remote)
			if err != nil {
				return nil, err
			}
		}
	} else if err != nil {
		return nil, err
	}
	updatedReq.Header = http.Header{}
	for k, v := range req.Header {
		if k != "Authorization" {
			updatedReq.Header[k] = v
		}
	}
	updatedReq.Header.Set("Authorization", "Bearer "+token)

	// Remove api_token=... from the the query string, in case we
	// end up forwarding the request.
	if values, err := url.ParseQuery(updatedReq.URL.RawQuery); err != nil {
		return nil, err
	} else if _, ok := values["api_token"]; ok {
		delete(values, "api_token")
		updatedReq.URL = &url.URL{
			Scheme:   req.URL.Scheme,
			Host:     req.URL.Host,
			Path:     req.URL.Path,
			RawPath:  req.URL.RawPath,
			RawQuery: values.Encode(),
		}
	}
	return updatedReq, nil
}
