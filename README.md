# Starting and Stopping Services as Mage Targets
[![Go Reference](https://pkg.go.dev/badge/github.com/swdunlop/mage-svc.svg)](https://pkg.go.dev/github.com/swdunlop/mage-svc)
[![Go Report Card](https://goreportcard.com/badge/github.com/swdunlop/mage-svc)](https://goreportcard.com/report/github.com/swdunlop/mage-svc)
[![Built with Mage](https://magefile.org/badge.svg)](https://magefile.org)

[Mage](https://magefile.org) is a build tool that supports writing targets in Go.  This package supports configuring
local services with local start and stop Mage targets.

## Example

The following Magefile running a local [NATS](https://nats.io) server for local testing:

```go
// +build mage

package main

import (
    "context.Context"
    "github.com/magefile/mage/mg"
    svc "github.com/swdunlop/mage-svc"
)

// Restart stops then starts a local NATS service for testing.
func Restart(ctx context.Context) { mg.SerialCtxDeps(ctx, Stop, Start) }

// Start a local NATS testing service.
func Start(ctx context.Context) { mg.CtxDeps(ctx, nats.Start()) }

// Stop the local NATS testing service.
func Stop(ctx context.Context) { mg.CtxDeps(ctx, nats.Stop()) }

// Status returns the status of the local NATS testing service.
func Status(ctx context.Context) { nats.Status(ctx).Print() }

// nats defines our local NATS service, which will run in "var/nats"
var nats = svc.New(`nats`,
    svc.Run(`nats-server`, `-addr`, `localhost`, `-m`, `8222`, `-js`, `-sd`, `.`),
    svc.Dir(`var/nats`),
    svc.DialCheck(`tcp`, `localhost:4222`),
    svc.HTTPCheck(`http://localhost:8222`, 200),
)
```
