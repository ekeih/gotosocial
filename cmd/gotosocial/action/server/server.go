// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"codeberg.org/gruf/go-sched"
	"github.com/gin-gonic/gin"
	"github.com/superseriousbusiness/gotosocial/cmd/gotosocial/action"
	"github.com/superseriousbusiness/gotosocial/internal/api"
	apiutil "github.com/superseriousbusiness/gotosocial/internal/api/util"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/middleware"
	tlprocessor "github.com/superseriousbusiness/gotosocial/internal/processing/timeline"
	"github.com/superseriousbusiness/gotosocial/internal/timeline"
	"github.com/superseriousbusiness/gotosocial/internal/tracing"
	"github.com/superseriousbusiness/gotosocial/internal/visibility"
	"go.uber.org/automaxprocs/maxprocs"

	"github.com/superseriousbusiness/gotosocial/internal/config"
	"github.com/superseriousbusiness/gotosocial/internal/db/bundb"
	"github.com/superseriousbusiness/gotosocial/internal/email"
	"github.com/superseriousbusiness/gotosocial/internal/federation"
	"github.com/superseriousbusiness/gotosocial/internal/federation/federatingdb"
	"github.com/superseriousbusiness/gotosocial/internal/gotosocial"
	"github.com/superseriousbusiness/gotosocial/internal/httpclient"
	"github.com/superseriousbusiness/gotosocial/internal/log"
	"github.com/superseriousbusiness/gotosocial/internal/media"
	"github.com/superseriousbusiness/gotosocial/internal/oauth"
	"github.com/superseriousbusiness/gotosocial/internal/oidc"
	"github.com/superseriousbusiness/gotosocial/internal/processing"
	"github.com/superseriousbusiness/gotosocial/internal/router"
	"github.com/superseriousbusiness/gotosocial/internal/state"
	gtsstorage "github.com/superseriousbusiness/gotosocial/internal/storage"
	"github.com/superseriousbusiness/gotosocial/internal/transport"
	"github.com/superseriousbusiness/gotosocial/internal/typeutils"
	"github.com/superseriousbusiness/gotosocial/internal/web"

	// Inherit memory limit if set from cgroup
	_ "github.com/KimMachineGun/automemlimit"
)

// Start creates and starts a gotosocial server
var Start action.GTSAction = func(ctx context.Context) error {
	if _, err := maxprocs.Set(maxprocs.Logger(nil)); err != nil {
		log.Infof(ctx, "could not set CPU limits from cgroup: %s", err)
	}

	var state state.State

	// Initialize caches
	state.Caches.Init()
	state.Caches.Start()
	defer state.Caches.Stop()

	// Initialize Tracing
	if err := tracing.Initialize(); err != nil {
		return fmt.Errorf("error initializing tracing: %w", err)
	}

	// Open connection to the database
	dbService, err := bundb.NewBunDBService(ctx, &state)
	if err != nil {
		return fmt.Errorf("error creating dbservice: %s", err)
	}

	// Set the state DB connection
	state.DB = dbService

	if err := dbService.CreateInstanceAccount(ctx); err != nil {
		return fmt.Errorf("error creating instance account: %s", err)
	}

	if err := dbService.CreateInstanceInstance(ctx); err != nil {
		return fmt.Errorf("error creating instance instance: %s", err)
	}

	// Open the storage backend
	storage, err := gtsstorage.AutoConfig()
	if err != nil {
		return fmt.Errorf("error creating storage backend: %w", err)
	}

	// Set the state storage driver
	state.Storage = storage

	// Build HTTP client
	client := httpclient.New(httpclient.Config{
		AllowRanges:           config.MustParseIPPrefixes(config.GetHTTPClientAllowIPs()),
		BlockRanges:           config.MustParseIPPrefixes(config.GetHTTPClientBlockIPs()),
		Timeout:               config.GetHTTPClientTimeout(),
		TLSInsecureSkipVerify: config.GetHTTPClientTLSInsecureSkipVerify(),
	})

	// Initialize workers.
	state.Workers.Start()
	defer state.Workers.Stop()

	// Add a task to the scheduler to sweep caches.
	// Frequency = 1 * minute
	// Threshold = 80% capacity
	sweep := func(time.Time) { state.Caches.Sweep(80) }
	job := sched.NewJob(sweep).Every(time.Minute)
	_ = state.Workers.Scheduler.Schedule(job)

	// Build handlers used in later initializations.
	mediaManager := media.NewManager(&state)
	oauthServer := oauth.New(ctx, dbService)
	typeConverter := typeutils.NewConverter(dbService)
	filter := visibility.NewFilter(&state)
	federatingDB := federatingdb.New(&state, typeConverter)
	transportController := transport.NewController(&state, federatingDB, &federation.Clock{}, client)
	federator := federation.NewFederator(&state, federatingDB, transportController, typeConverter, mediaManager)

	// Decide whether to create a noop email
	// sender (won't send emails) or a real one.
	var emailSender email.Sender
	if smtpHost := config.GetSMTPHost(); smtpHost != "" {
		// Host is defined; create a proper sender.
		emailSender, err = email.NewSender()
		if err != nil {
			return fmt.Errorf("error creating email sender: %s", err)
		}
	} else {
		// No host is defined; create a noop sender.
		emailSender, err = email.NewNoopSender(nil)
		if err != nil {
			return fmt.Errorf("error creating noop email sender: %s", err)
		}
	}

	// Initialize timelines.
	state.Timelines.Home = timeline.NewManager(
		tlprocessor.HomeTimelineGrab(&state),
		tlprocessor.HomeTimelineFilter(&state, filter),
		tlprocessor.HomeTimelineStatusPrepare(&state, typeConverter),
		tlprocessor.SkipInsert(),
	)
	if err := state.Timelines.Home.Start(); err != nil {
		return fmt.Errorf("error starting home timeline: %s", err)
	}

	state.Timelines.List = timeline.NewManager(
		tlprocessor.ListTimelineGrab(&state),
		tlprocessor.ListTimelineFilter(&state, filter),
		tlprocessor.ListTimelineStatusPrepare(&state, typeConverter),
		tlprocessor.SkipInsert(),
	)
	if err := state.Timelines.List.Start(); err != nil {
		return fmt.Errorf("error starting list timeline: %s", err)
	}

	// Create the processor using all the other services we've created so far.
	processor := processing.NewProcessor(typeConverter, federator, oauthServer, mediaManager, &state, emailSender)

	// Set state client / federator worker enqueue functions
	state.Workers.EnqueueClientAPI = processor.EnqueueClientAPI
	state.Workers.EnqueueFederator = processor.EnqueueFederator

	/*
		HTTP router initialization
	*/

	router, err := router.New(ctx)
	if err != nil {
		return fmt.Errorf("error creating router: %s", err)
	}

	middlewares := []gin.HandlerFunc{
		middleware.AddRequestID(config.GetRequestIDHeader()), // requestID middleware must run before tracing
	}
	if config.GetTracingEnabled() {
		middlewares = append(middlewares, tracing.InstrumentGin())
	}
	middlewares = append(middlewares, []gin.HandlerFunc{
		// note: hooks adding ctx fields must be ABOVE
		// the logger, otherwise won't be accessible.
		middleware.Logger(config.GetLogClientIP()),
		middleware.UserAgent(),
		middleware.CORS(),
		middleware.ExtraHeaders(),
	}...)

	// attach global middlewares which are used for every request
	router.AttachGlobalMiddleware(middlewares...)

	// attach global no route / 404 handler to the router
	router.AttachNoRouteHandler(func(c *gin.Context) {
		apiutil.ErrorHandler(c, gtserror.NewErrorNotFound(errors.New(http.StatusText(http.StatusNotFound))), processor.InstanceGetV1)
	})

	// build router modules
	var idp oidc.IDP
	if config.GetOIDCEnabled() {
		idp, err = oidc.NewIDP(ctx)
		if err != nil {
			return fmt.Errorf("error creating oidc idp: %w", err)
		}
	}

	routerSession, err := dbService.GetSession(ctx)
	if err != nil {
		return fmt.Errorf("error retrieving router session for session middleware: %w", err)
	}

	sessionName, err := middleware.SessionName()
	if err != nil {
		return fmt.Errorf("error generating session name for session middleware: %w", err)
	}

	var (
		authModule        = api.NewAuth(dbService, processor, idp, routerSession, sessionName) // auth/oauth paths
		clientModule      = api.NewClient(dbService, processor)                                // api client endpoints
		fileserverModule  = api.NewFileserver(processor)                                       // fileserver endpoints
		wellKnownModule   = api.NewWellKnown(processor)                                        // .well-known endpoints
		nodeInfoModule    = api.NewNodeInfo(processor)                                         // nodeinfo endpoint
		activityPubModule = api.NewActivityPub(dbService, processor)                           // ActivityPub endpoints
		webModule         = web.New(dbService, processor)                                      // web pages + user profiles + settings panels etc
	)

	// create required middleware
	// rate limiting
	limit := config.GetAdvancedRateLimitRequests()
	clLimit := middleware.RateLimit(limit)  // client api
	s2sLimit := middleware.RateLimit(limit) // server-to-server (AP)
	fsLimit := middleware.RateLimit(limit)  // fileserver / web templates

	// throttling
	cpuMultiplier := config.GetAdvancedThrottlingMultiplier()
	retryAfter := config.GetAdvancedThrottlingRetryAfter()
	clThrottle := middleware.Throttle(cpuMultiplier, retryAfter)  // client api
	s2sThrottle := middleware.Throttle(cpuMultiplier, retryAfter) // server-to-server (AP)
	fsThrottle := middleware.Throttle(cpuMultiplier, retryAfter)  // fileserver / web templates
	pkThrottle := middleware.Throttle(cpuMultiplier, retryAfter)  // throttle public key endpoint separately

	gzip := middleware.Gzip() // applied to all except fileserver

	// these should be routed in order;
	// apply throttling *after* rate limiting
	authModule.Route(router, clLimit, clThrottle, gzip)
	clientModule.Route(router, clLimit, clThrottle, gzip)
	fileserverModule.Route(router, fsLimit, fsThrottle)
	wellKnownModule.Route(router, gzip, s2sLimit, s2sThrottle)
	nodeInfoModule.Route(router, s2sLimit, s2sThrottle, gzip)
	activityPubModule.Route(router, s2sLimit, s2sThrottle, gzip)
	activityPubModule.RoutePublicKey(router, s2sLimit, pkThrottle, gzip)
	webModule.Route(router, fsLimit, fsThrottle, gzip)

	gts, err := gotosocial.NewServer(dbService, router, federator, mediaManager)
	if err != nil {
		return fmt.Errorf("error creating gotosocial service: %s", err)
	}

	if err := gts.Start(ctx); err != nil {
		return fmt.Errorf("error starting gotosocial service: %s", err)
	}

	// catch shutdown signals from the operating system
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigs // block until signal received
	log.Infof(ctx, "received signal %s, shutting down", sig)

	// close down all running services in order
	if err := gts.Stop(ctx); err != nil {
		return fmt.Errorf("error closing gotosocial service: %s", err)
	}

	log.Info(ctx, "done! exiting...")
	return nil
}
