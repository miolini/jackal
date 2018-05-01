/*
 * Copyright (c) 2018 Miguel Ángel Ortuño.
 * See the LICENSE file for more information.
 */

package module

import (
	"github.com/ortuman/jackal/log"
	"github.com/ortuman/jackal/storage"
	"github.com/ortuman/jackal/storage/model"
	"github.com/ortuman/jackal/stream/c2s"
	"github.com/ortuman/jackal/xml"
	"github.com/pborman/uuid"
)

const blockingCommandNamespace = "urn:xmpp:blocking"

const (
	xep191RequestedContextKey = "xep_191:requested"
)

// XEPBlockingCommand returns a blocking command IQ handler module.
type XEPBlockingCommand struct {
	stm c2s.Stream
}

// NewXEPBlockingCommand returns a blocking command IQ handler module.
func NewXEPBlockingCommand(stm c2s.Stream) *XEPBlockingCommand {
	return &XEPBlockingCommand{stm: stm}
}

// AssociatedNamespaces returns namespaces associated
// with blocking command module.
func (x *XEPBlockingCommand) AssociatedNamespaces() []string {
	return []string{blockingCommandNamespace}
}

// Done signals stream termination.
func (x *XEPBlockingCommand) Done() {
}

// MatchesIQ returns whether or not an IQ should be
// processed by the blocking command module.
func (x *XEPBlockingCommand) MatchesIQ(iq *xml.IQ) bool {
	e := iq.Elements()
	blockList := e.ChildNamespace("blocklist", blockingCommandNamespace)
	block := e.ChildNamespace("block", blockingCommandNamespace)
	unblock := e.ChildNamespace("unblock", blockingCommandNamespace)
	return (iq.IsGet() && blockList != nil) || (iq.IsSet() && (block != nil || unblock != nil))
}

// ProcessIQ processes a blocking command IQ taking according actions
// over the associated stream.
func (x *XEPBlockingCommand) ProcessIQ(iq *xml.IQ) {
	if iq.IsGet() {
		x.sendBlockList(iq)
	} else if iq.IsSet() {
		e := iq.Elements()
		if block := e.ChildNamespace("block", blockingCommandNamespace); block != nil {
			x.block(iq, block)
		} else if unblock := e.ChildNamespace("unblock", blockingCommandNamespace); unblock != nil {
			x.unblock(iq, unblock)
		}
	}
}

func (x *XEPBlockingCommand) sendBlockList(iq *xml.IQ) {
	blItms, err := storage.Instance().FetchBlockListItems(x.stm.Username())
	if err != nil {
		log.Error(err)
		x.stm.SendElement(iq.InternalServerError())
		return
	}
	blockList := xml.NewElementNamespace("blocklist", blockingCommandNamespace)
	for _, blItm := range blItms {
		itElem := xml.NewElementName("item")
		itElem.SetAttribute("jid", blItm.JID)
		blockList.AppendElement(itElem)
	}
	reply := iq.ResultIQ()
	reply.AppendElement(blockList)
	x.stm.SendElement(reply)

	x.stm.Context().SetBool(true, xep191RequestedContextKey)
}

func (x *XEPBlockingCommand) block(iq *xml.IQ, block xml.XElement) {
	items := block.Elements().Children("item")
	if len(items) == 0 {
		x.stm.SendElement(iq.BadRequestError())
		return
	}
	jds, err := x.extractItemJIDs(items)
	if err != nil {
		log.Error(err)
		x.stm.SendElement(iq.JidMalformedError())
		return
	}
	ris, _, err := storage.Instance().FetchRosterItems(x.stm.Username())
	if err != nil {
		log.Error(err)
		x.stm.SendElement(iq.InternalServerError())
		return
	}
	blItms, err := storage.Instance().FetchBlockListItems(x.stm.Username())
	if err != nil {
		log.Error(err)
		x.stm.SendElement(iq.InternalServerError())
		return
	}
	var bl []model.BlockListItem
	for _, j := range jds {
		if !x.isJIDInBlockList(j, blItms) {
			x.broadcastPresenceMatching(j, ris, xml.UnavailableType)
			bl = append(bl, model.BlockListItem{Username: x.stm.Username(), JID: j.String()})
		}
	}
	if len(bl) > 0 {
		if err := storage.Instance().InsertOrUpdateBlockListItems(bl); err != nil {
			log.Error(err)
			x.stm.SendElement(iq.InternalServerError())
			return
		}
	}
	x.stm.SendElement(iq.ResultIQ())
	x.pushIQ(block)

	c2s.Instance().ReloadBlockList(x.stm.Username())
}

func (x *XEPBlockingCommand) unblock(iq *xml.IQ, unblock xml.XElement) {
	ris, _, err := storage.Instance().FetchRosterItems(x.stm.Username())
	if err != nil {
		log.Error(err)
		x.stm.SendElement(iq.InternalServerError())
		return
	}
	blItms, err := storage.Instance().FetchBlockListItems(x.stm.Username())
	if err != nil {
		log.Error(err)
		x.stm.SendElement(iq.InternalServerError())
		return
	}
	items := unblock.Elements().Children("item")
	if len(items) == 0 {
		if err := storage.Instance().DeleteBlockList(x.stm.Username()); err != nil {
			log.Error(err)
			x.stm.SendElement(iq.InternalServerError())
			return
		}
		for _, blItm := range blItms {
			j, _ := xml.NewJIDString(blItm.JID, true)
			x.broadcastPresenceMatching(j, ris, xml.AvailableType)
		}

	} else {
		jds, err := x.extractItemJIDs(items)
		if err != nil {
			log.Error(err)
			x.stm.SendElement(iq.JidMalformedError())
			return
		}
		var bl []model.BlockListItem
		for _, j := range jds {
			if x.isJIDInBlockList(j, blItms) {
				x.broadcastPresenceMatching(j, ris, xml.AvailableType)
				bl = append(bl, model.BlockListItem{Username: x.stm.Username(), JID: j.String()})
			}
		}
		if err := storage.Instance().DeleteBlockListItems(bl); err != nil {
			log.Error(err)
			x.stm.SendElement(iq.InternalServerError())
			return
		}
	}
	x.stm.SendElement(iq.ResultIQ())
	x.pushIQ(unblock)

	c2s.Instance().ReloadBlockList(x.stm.Username())
}

func (x *XEPBlockingCommand) pushIQ(elem xml.XElement) {
	stms := c2s.Instance().StreamsMatchingJID(x.stm.JID().ToBareJID())
	for _, stm := range stms {
		if !stm.Context().Bool(xep191RequestedContextKey) {
			continue
		}
		iq := xml.NewIQType(uuid.New(), xml.SetType)
		iq.AppendElement(elem)
		stm.SendElement(iq)
	}
}

func (x *XEPBlockingCommand) broadcastPresenceMatching(jid *xml.JID, ris []model.RosterItem, presenceType string) {
	stms := c2s.Instance().StreamsMatchingJID(jid)
	for _, stm := range stms {
		if !x.isSubscribedFrom(jid, ris) {
			continue
		}
		p := xml.NewPresence(stm.JID(), x.stm.JID().ToBareJID(), presenceType)
		if presence := stm.Presence(); presence != nil && presenceType == xml.AvailableType {
			p.AppendElements(presence.Elements().All())
		}
		c2s.Instance().Route(p)
	}
}

func (x *XEPBlockingCommand) isJIDInBlockList(jid *xml.JID, blItems []model.BlockListItem) bool {
	for _, blItem := range blItems {
		if blItem.JID == jid.String() {
			return true
		}
	}
	return false
}

func (x *XEPBlockingCommand) isSubscribedFrom(jid *xml.JID, ris []model.RosterItem) bool {
	str := jid.String()
	for _, ri := range ris {
		if ri.JID == str && (ri.Subscription == subscriptionFrom || ri.Subscription == subscriptionBoth) {
			return true
		}
	}
	return false
}

func (x *XEPBlockingCommand) extractItemJIDs(items []xml.XElement) ([]*xml.JID, error) {
	var ret []*xml.JID
	for _, item := range items {
		j, err := xml.NewJIDString(item.Attributes().Get("jid"), false)
		if err != nil {
			return nil, err
		}
		ret = append(ret, j)
	}
	return ret, nil
}
