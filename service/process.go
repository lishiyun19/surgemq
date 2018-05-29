// Copyright (c) 2018 Zentures, LLC. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"errors"
	"fmt"
	"io"
	"reflect"

	""
	"code.surgemq.com/messages"
	"code.surgemq.com/sessions"
)

var (
	errDisconnect = errors.New("Disconnect")
)

// processor() reads messages from the incoming buffer and processes them
func (this *service) processor() {
	defer func() {
		// Let's recover from panic
		if r := recover(); r != nil {
			//commons.Log.Error("(%s) Recovering from panic: %v", this.cid(), r)
		}

		this.wgStopped.Done()
		this.stop()

		//commons.Log.Debug("(%s) Stopping processor", this.cid())
	}()

	commons.Log.Debug("(%s) Starting processor", this.cid())

	this.wgStarted.Done()

	for {
		// 1. Find out what message is next and the size of the message
		mtype, total, err := this.peekMessageSize()
		if err != nil {
			//if err != io.EOF {
			commons.Log.Error("(%s) Error peeking next message size: %v", this.cid(), err)
			//}
			return
		}

		msg, n, err := this.peekMessage(mtype, total)
		if err != nil {
			//if err != io.EOF {
			commons.Log.Error("(%s) Error peeking next message: %v", this.cid(), err)
			//}
			return
		}

		//commons.Log.Debug("(%s) Received: %s", this.cid(), msg)

		this.inStat.increment(int64(n))

		// 5. Process the read message
		err = this.processIncoming(msg)
		if err != nil {
			if err != errDisconnect {
				commons.Log.Error("(%s) Error processing %s: %v", this.cid(), msg.Name(), err)
			} else {
				return
			}
		}

		// 7. We should commit the bytes in the buffer so we can move on
		_, err = this.in.ReadCommit(total)
		if err != nil {
			if err != io.EOF {
				commons.Log.Error("(%s) Error committing %d read bytes: %v", this.cid(), total, err)
			}
			return
		}

		// 7. Check to see if done is closed, if so, exit
		if this.isDone() && this.in.Len() == 0 {
			return
		}

		//if this.inStat.msgs%1000 == 0 {
		//	commons.Log.Debug("(%s) Going to process message %d", this.cid(), this.inStat.msgs)
		//}
	}
}

func (this *service) processIncoming(msg messages.Message) error {
	var err error = nil

	switch msg := msg.(type) {
	case *messages.PublishMessage:
		// For PUBLISH message, we should figure out what QoS it is and process accordingly
		// If QoS == 0, we should just take the next step, no ack required
		// If QoS == 1, we should send back PUBACK, then take the next step
		// If QoS == 2, we need to put it in the ack queue, send back PUBREC
		err = this.processPublish(msg)

	case *messages.PubackMessage:
		// For PUBACK message, it means QoS 1, we should send to ack queue
		this.sess.Pub1ack.Ack(msg)
		this.processAcked(this.sess.Pub1ack)

	case *messages.PubrecMessage:
		// For PUBREC message, it means QoS 2, we should send to ack queue, and send back PUBREL
		if err = this.sess.Pub2out.Ack(msg); err != nil {
			break
		}

		resp := messages.NewPubrelMessage()
		resp.SetPacketId(msg.PacketId())
		_, err = this.writeMessage(resp)

	case *messages.PubrelMessage:
		// For PUBREL message, it means QoS 2, we should send to ack queue, and send back PUBCOMP
		if err = this.sess.Pub2in.Ack(msg); err != nil {
			break
		}

		this.processAcked(this.sess.Pub2in)

		resp := messages.NewPubcompMessage()
		resp.SetPacketId(msg.PacketId())
		_, err = this.writeMessage(resp)

	case *messages.PubcompMessage:
		// For PUBCOMP message, it means QoS 2, we should send to ack queue
		if err = this.sess.Pub2out.Ack(msg); err != nil {
			break
		}

		this.processAcked(this.sess.Pub2out)

	case *messages.SubscribeMessage:
		// For SUBSCRIBE message, we should add subscriber, then send back SUBACK
		return this.processSubscribe(msg)

	case *messages.SubackMessage:
		// For SUBACK message, we should send to ack queue
		this.sess.Suback.Ack(msg)
		this.processAcked(this.sess.Suback)

	case *messages.UnsubscribeMessage:
		// For UNSUBSCRIBE message, we should remove subscriber, then send back UNSUBACK
		return this.processUnsubscribe(msg)

	case *messages.UnsubackMessage:
		// For UNSUBACK message, we should send to ack queue
		this.sess.Unsuback.Ack(msg)
		this.processAcked(this.sess.Unsuback)

	case *messages.PingreqMessage:
		// For PINGREQ message, we should send back PINGRESP
		resp := messages.NewPingrespMessage()
		_, err = this.writeMessage(resp)

	case *messages.PingrespMessage:
		this.sess.Pingack.Ack(msg)
		this.processAcked(this.sess.Pingack)

	case *messages.DisconnectMessage:
		// For DISCONNECT message, we should quit
		this.sess.Cmsg.SetWillFlag(false)
		return errDisconnect

	default:
		return fmt.Errorf("(%s) invalid message type %s.", this.cid(), msg.Name())
	}

	if err != nil {
		commons.Log.Debug("(%s) Error processing acked message: %v", this.cid(), err)
	}

	return err
}

func (this *service) processAcked(ackq *sessions.Ackqueue) {
	for _, ackmsg := range ackq.Acked() {
		// Let's get the messages from the saved message byte slices.
		msg, err := ackmsg.Mtype.New()
		if err != nil {
			commons.Log.Error("process/processAcked: Unable to creating new %s message: %v", ackmsg.Mtype, err)
			continue
		}

		if _, err := msg.Decode(ackmsg.Msgbuf); err != nil {
			commons.Log.Error("process/processAcked: Unable to decode %s message: %v", ackmsg.Mtype, err)
			continue
		}

		ack, err := ackmsg.State.New()
		if err != nil {
			commons.Log.Error("process/processAcked: Unable to creating new %s message: %v", ackmsg.State, err)
			continue
		}

		if _, err := ack.Decode(ackmsg.Ackbuf); err != nil {
			commons.Log.Error("process/processAcked: Unable to decode %s message: %v", ackmsg.State, err)
			continue
		}

		//commons.Log.Debug("(%s) Processing acked message: %v", this.cid(), ack)

		// - PUBACK if it's QoS 1 messages. This is on the client side.
		// - PUBREL if it's QoS 2 messages. This is on the server side.
		// - PUBCOMP if it's QoS 2 messages. This is on the client side.
		// - SUBACK if it's a subscribe messages. This is on the client side.
		// - UNSUBACK if it's a unsubscribe messages. This is on the client side.
		switch ackmsg.State {
		case messages.PUBREL:
			// If ack is PUBREL, that means the QoS 2 message sent by a remote client is
			// releassed, so let's publish it to other subscribers.
			if err = this.onPublish(msg.(*messages.PublishMessage)); err != nil {
				commons.Log.Error("(%s) Error processing ack'ed %s message: %v", this.cid(), ackmsg.Mtype, err)
			}

		case messages.PUBACK, messages.PUBCOMP, messages.SUBACK, messages.UNSUBACK, messages.PINGRESP:
			commons.Log.Debug("process/processAcked: %s", ack)
			// If ack is PUBACK, that means the QoS 1 message sent by this service got
			// ack'ed. There's nothing to do other than calling onComplete() below.

			// If ack is PUBCOMP, that means the QoS 2 message sent by this service got
			// ack'ed. There's nothing to do other than calling onComplete() below.

			// If ack is SUBACK, that means the SUBSCRIBE message sent by this service
			// got ack'ed. There's nothing to do other than calling onComplete() below.

			// If ack is UNSUBACK, that means the SUBSCRIBE message sent by this service
			// got ack'ed. There's nothing to do other than calling onComplete() below.

			// If ack is PINGRESP, that means the PINGREQ message sent by this service
			// got ack'ed. There's nothing to do other than calling onComplete() below.

			err = nil

		default:
			commons.Log.Error("(%s) Invalid ack message type %s.", this.cid(), ackmsg.State)
			continue
		}

		// Call the registered onComplete function
		if ackmsg.OnComplete != nil {
			onComplete, ok := ackmsg.OnComplete.(OnCompleteFunc)
			if !ok {
				commons.Log.Error("process/processAcked: Error type asserting onComplete function: %v", reflect.TypeOf(ackmsg.OnComplete))
			} else if onComplete != nil {
				if err := onComplete(msg, ack, nil); err != nil {
					commons.Log.Error("process/processAcked: Error running onComplete(): %v", err)
				}
			}
		}
	}
}

// For PUBLISH message, we should figure out what QoS it is and process accordingly
// If QoS == 0, we should just take the next step, no ack required
// If QoS == 1, we should send back PUBACK, then take the next step
// If QoS == 2, we need to put it in the ack queue, send back PUBREC
func (this *service) processPublish(msg *messages.PublishMessage) error {
	switch msg.QoS() {
	case messages.QosExactlyOnce:
		this.sess.Pub2in.Wait(msg, nil)

		resp := messages.NewPubrecMessage()
		resp.SetPacketId(msg.PacketId())

		_, err := this.writeMessage(resp)
		return err

	case messages.QosAtLeastOnce:
		resp := messages.NewPubackMessage()
		resp.SetPacketId(msg.PacketId())

		if _, err := this.writeMessage(resp); err != nil {
			return err
		}

		return this.onPublish(msg)

	case messages.QosAtMostOnce:
		return this.onPublish(msg)
	}

	return fmt.Errorf("(%s) invalid message QoS %d.", this.cid(), msg.QoS())
}

// For SUBSCRIBE message, we should add subscriber, then send back SUBACK
func (this *service) processSubscribe(msg *messages.SubscribeMessage) error {
	resp := messages.NewSubackMessage()
	resp.SetPacketId(msg.PacketId())

	// Subscribe to the different topics
	var retcodes []byte

	topics := msg.Topics()
	qos := msg.Qos()

	this.rmsgs = this.rmsgs[0:0]

	for i, t := range topics {
		rqos, err := this.topicsMgr.Subscribe(t, qos[i], &this.onpub)
		if err != nil {
			return err
		}
		this.sess.AddTopic(string(t), qos[i])

		retcodes = append(retcodes, rqos)

		// yeah I am not checking errors here. If there's an error we don't want the
		// subscription to stop, just let it go.
		this.topicsMgr.Retained(t, &this.rmsgs)
		commons.Log.Debug("(%s) topic = %s, retained count = %d", this.cid(), string(t), len(this.rmsgs))
	}

	if err := resp.AddReturnCodes(retcodes); err != nil {
		return err
	}

	if _, err := this.writeMessage(resp); err != nil {
		return err
	}

	for _, rm := range this.rmsgs {
		if err := this.publish(rm, nil); err != nil {
			commons.Log.Error("service/processSubscribe: Error publishing retained message: %v", err)
			return err
		}
	}

	return nil
}

// For UNSUBSCRIBE message, we should remove the subscriber, and send back UNSUBACK
func (this *service) processUnsubscribe(msg *messages.UnsubscribeMessage) error {
	topics := msg.Topics()

	for _, t := range topics {
		this.topicsMgr.Unsubscribe(t, &this.onpub)
		this.sess.RemoveTopic(string(t))
	}

	resp := messages.NewUnsubackMessage()
	resp.SetPacketId(msg.PacketId())

	_, err := this.writeMessage(resp)
	return err
}

// onPublish() is called when the server receives a PUBLISH message AND have completed
// the ack cycle. This method will get the list of subscribers based on the publish
// topic, and publishes the message to the list of subscribers.
func (this *service) onPublish(msg *messages.PublishMessage) error {
	if msg.Retain() {
		if err := this.topicsMgr.Retain(msg); err != nil {
			commons.Log.Error("(%s) Error retaining message: %v", this.cid(), err)
		}
	}

	err := this.topicsMgr.Subscribers(msg.Topic(), msg.QoS(), &this.subs, &this.qoss)
	if err != nil {
		commons.Log.Error("(%s) Error retrieving subscribers list: %v", this.cid(), err)
		return err
	}

	msg.SetRetain(false)

	//commons.Log.Debug("(%s) Publishing to topic %q and %d subscribers", this.cid(), string(msg.Topic()), len(this.subs))
	for _, s := range this.subs {
		if s != nil {
			fn, ok := s.(*OnPublishFunc)
			if !ok {
				commons.Log.Error("Invalid onPublish Function")
				return fmt.Errorf("Invalid onPublish Function")
			} else {
				(*fn)(msg)
			}
		}
	}

	return nil
}
