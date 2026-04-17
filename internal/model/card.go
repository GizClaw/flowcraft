package model

import (
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/kanban"
)

type CardStatus = kanban.CardStatus

const (
	CardPending = kanban.CardPending
	CardClaimed = kanban.CardClaimed
	CardDone    = kanban.CardDone
	CardFailed  = kanban.CardFailed
)

type Card = kanban.Card
type CardFilter = kanban.CardFilter
type CardOption = kanban.CardOption

var WithConsumer = kanban.WithConsumer
var WithMeta = kanban.WithMeta

type BoardSnapshot = graph.BoardSnapshot
