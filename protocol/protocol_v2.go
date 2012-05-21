package protocol

import (
	"../message"
	"../util"
	"bufio"
	"bytes"
	"encoding/binary"
	"log"
	"reflect"
	"strconv"
	"strings"
)

const (
	ClientStateV2Init       = 0
	ClientStateV2Subscribed = 1
)

const (
	FrameTypeResponse = 0
	FrameTypeError    = 1
	FrameTypeMessage  = 2
)

var (
	ClientErrV2Invalid    = ClientError{"E_INVALID"}
	ClientErrV2BadTopic   = ClientError{"E_BAD_TOPIC"}
	ClientErrV2BadChannel = ClientError{"E_BAD_CHANNEL"}
	ClientErrV2BadMessage = ClientError{"E_BAD_MESSAGE"}
)

func init() {
	// BigEndian client byte sequence "  V2"
	Protocols[538990130] = &ProtocolV2{}
}

type ProtocolV2 struct{}

func (p *ProtocolV2) IOLoop(client StatefulReadWriter) error {
	var err error
	var line string

	clientExitChan := make(chan int)
	client.SetState("state", ClientStateV2Init)
	client.SetState("exit_chan", clientExitChan)

	err = nil
	reader := bufio.NewReader(client)
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			break
		}

		line = strings.Replace(line, "\n", "", -1)
		line = strings.Replace(line, "\r", "", -1)
		params := strings.Split(line, " ")

		log.Printf("PROTOCOL(V2): %#v", params)

		response, err := p.Execute(client, params...)
		if err != nil {
			clientData, err := p.Frame(FrameTypeError, []byte(err.Error()))
			if err != nil {
				break
			}

			_, err = client.Write(clientData)
			if err != nil {
				break
			}
			continue
		}

		if response != nil {
			clientData, err := p.Frame(FrameTypeResponse, response)
			if err != nil {
				break
			}

			_, err = client.Write(clientData)
			if err != nil {
				break
			}
		}
	}

	clientExitChan <- 1

	return err
}

func (p *ProtocolV2) Frame(frameType int32, data []byte) ([]byte, error) {
	var buf bytes.Buffer
	var err error

	err = binary.Write(&buf, binary.BigEndian, &frameType)
	if err != nil {
		return nil, err
	}

	_, err = buf.Write(data)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (p *ProtocolV2) Execute(client StatefulReadWriter, params ...string) ([]byte, error) {
	var err error
	var response []byte

	typ := reflect.TypeOf(p)
	args := make([]reflect.Value, 3)
	args[0] = reflect.ValueOf(p)
	args[1] = reflect.ValueOf(client)

	cmd := strings.ToUpper(params[0])

	// use reflection to call the appropriate method for this 
	// command on the protocol object
	if method, ok := typ.MethodByName(cmd); ok {
		args[2] = reflect.ValueOf(params)
		returnValues := method.Func.Call(args)
		response = nil
		if !returnValues[0].IsNil() {
			response = returnValues[0].Interface().([]byte)
		}
		err = nil
		if !returnValues[1].IsNil() {
			err = returnValues[1].Interface().(error)
		}

		return response, err
	}

	return nil, ClientErrV2Invalid
}

func (p *ProtocolV2) PushMessages(client StatefulReadWriter) {
	var err error

	client.SetState("ready_count", 0)

	readyStateChanInterface, _ := client.GetState("ready_state_chan")
	readyStateChan := readyStateChanInterface.(chan int)

	clientExitChanInterface, _ := client.GetState("exit_chan")
	clientExitChan := clientExitChanInterface.(chan int)

	channelInterface, _ := client.GetState("channel")
	channel := channelInterface.(*message.Channel)

	for {
		readyCountInterface, _ := client.GetState("ready_count")
		readyCount := readyCountInterface.(int)
		if readyCount > 0 {
			select {
			case count := <-readyStateChan:
				client.SetState("ready_count", count)
			case msg := <-channel.ClientMessageChan:
				client.SetState("ready_count", readyCount-1)

				log.Printf("PROTOCOL(V2): writing msg(%s) to client(%s) - %s",
					util.UuidToStr(msg.Uuid()), client.String(), string(msg.Body()))

				buf := new(bytes.Buffer)

				_, err = buf.Write(msg.Uuid())
				if err != nil {
					goto exit
				}

				_, err = buf.Write(msg.Body())
				if err != nil {
					goto exit
				}

				clientData, err := p.Frame(FrameTypeMessage, buf.Bytes())
				if err != nil {
					goto exit
				}

				client.Write(clientData)
			case <-clientExitChan:
				goto exit
			}
		} else {
			select {
			case count := <-readyStateChan:
				client.SetState("ready_count", count)
			case <-clientExitChan:
				goto exit
			}
		}
	}

exit:
	if err != nil {
		log.Printf("PROTOCOL(V2): PushMessages error - %s", err.Error())
	}
}

func (p *ProtocolV2) SUB(client StatefulReadWriter, params []string) ([]byte, error) {
	if state, _ := client.GetState("state"); state.(int) != ClientStateV2Init {
		return nil, ClientErrV2Invalid
	}

	if len(params) < 3 {
		return nil, ClientErrV2Invalid
	}

	topicName := params[1]
	if len(topicName) == 0 {
		return nil, ClientErrV2BadTopic
	}

	channelName := params[2]
	if len(channelName) == 0 {
		return nil, ClientErrV2BadChannel
	}

	readyStateChan := make(chan int)
	client.SetState("ready_state_chan", readyStateChan)

	topic := message.GetTopic(topicName)
	client.SetState("channel", topic.GetChannel(channelName))

	client.SetState("state", ClientStateV2Subscribed)

	go p.PushMessages(client)

	return nil, nil
}

func (p *ProtocolV2) RDY(client StatefulReadWriter, params []string) ([]byte, error) {
	var err error

	if state, _ := client.GetState("state"); state.(int) != ClientStateV2Subscribed {
		return nil, ClientErrV2Invalid
	}

	count := 1
	if len(params) > 1 {
		count, err = strconv.Atoi(params[1])
		if err != nil {
			return nil, err
		}
	}

	if count > 1000 {
		return nil, ClientErrV2Invalid
	}

	readyStateChanInterface, _ := client.GetState("ready_state_chan")
	readyStateChan := readyStateChanInterface.(chan int)
	readyStateChan <- count

	return nil, nil
}

func (p *ProtocolV2) FIN(client StatefulReadWriter, params []string) ([]byte, error) {
	if state, _ := client.GetState("state"); state.(int) != ClientStateV2Subscribed {
		return nil, ClientErrV2Invalid
	}

	if len(params) < 2 {
		return nil, ClientErrV2Invalid
	}

	uuidStr := params[1]
	channelInterface, _ := client.GetState("channel")
	channel := channelInterface.(*message.Channel)
	err := channel.FinishMessage(uuidStr)
	if err != nil {
		return nil, err
	}

	return nil, nil
}

func (p *ProtocolV2) REQ(client StatefulReadWriter, params []string) ([]byte, error) {
	if state, _ := client.GetState("state"); state.(int) != ClientStateV2Subscribed {
		return nil, ClientErrV2Invalid
	}

	if len(params) < 2 {
		return nil, ClientErrV2Invalid
	}

	uuidStr := params[1]
	channelInterface, _ := client.GetState("channel")
	channel := channelInterface.(*message.Channel)
	err := channel.RequeueMessage(uuidStr)
	if err != nil {
		return nil, err
	}

	return nil, nil
}
