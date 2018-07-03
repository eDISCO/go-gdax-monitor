package main

import (
	"fmt"
	//"github.com/davecgh/go-spew/spew"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	//"github.com/fatih/structs"
	ws "github.com/gorilla/websocket"
	exchange "github.com/preichenberger/go-coinbase-exchange"
	"github.com/tidwall/buntdb"
	"github.com/tidwall/gjson"
	"log"
	//"os"
	ui "github.com/gizak/termui"
	//"math"
	"strconv"
	"time"
)

const (
	// This behaviour is unsafe, always pass API keys as env
	secret     = "SecretHERE"
	key        = "KeyHERE"
	passphrase = "PassPhaseHERE"
	product    = "BTC-USD"
)

var (
	highest_bid    float64 = 1
	lowest_ask             = 1e6
	historic_rates []exchange.HistoricRate
	rates_lock     bool = false
)

type InternalBook struct {
	Price          float64
	Size           float64
	NumberOfOrders int
	OrderId        string
}

func main() {
	// dispatch the websocket listener first
	ws_updates := make(chan exchange.Message, 400)
	semaphor := make(chan bool)
	go web_socket(ws_updates, semaphor)
	// Init 2 DBs, one for Ask, one for Bid
	// let's keep bid and ask parts of the book separate?

	Ask, err := buntdb.Open(":memory:")
	if err != nil {
		log.Fatal(err)
	}
	defer Ask.Close()
	Ask.CreateIndex("Price", "*", buntdb.IndexJSON("Price"))
	Bid, err := buntdb.Open(":memory:")
	if err != nil {
		log.Fatal(err)
	}
	defer Bid.Close()
	Bid.CreateIndex("Price", "*", buntdb.IndexJSON("Price"))
	///

	// get the latest book snapshot second
	client := exchange.NewClient(secret, key, passphrase)
	var book exchange.Book
	_ = <-semaphor
	book, err = client.GetBook(product, 3)
	sequence := book.Sequence
	if err != nil {
		log.Print(err)
	}
	// get historic rates and volumes for last 24 hours
	params := exchange.GetHistoricRatesParams{time.Now().Add(-time.Hour * 24), time.Now(), 300}
	historic_rates, err = client.GetHistoricRates(product, params)
	if err!=nil{
		log.Fatal("Historic rates failed: ", err)
		}
	// insert book to the db
	Ask.Update(func(tx *buntdb.Tx) error {

		for _, ask_struct := range book.Asks {
			bytes, err := json.Marshal(ask_struct)
			if err != nil {
				log.Fatal(err)
			}
			tx.Set(ask_struct.OrderId, string(bytes), nil)
		}

		return nil
	})
	Bid.Update(func(tx *buntdb.Tx) error {

		for _, bid_struct := range book.Bids {
			bytes, err := json.Marshal(bid_struct)
			if err != nil {
				log.Fatal(err)
			}
			tx.Set(bid_struct.OrderId, string(bytes), nil)
		}

		return nil
	})
	go Wait_for_updates(Ask, Bid, ws_updates, sequence)
	go Start_gui(Ask, Bid)
	for true {

		//fmt.Println("The spread is: ", spread)
		time.Sleep(2000 * time.Millisecond)
		_ = calculate_spread(Ask, Bid)
		//log.Printf("%.2f \t %.2f \t %.2f", lowest_ask, highest_bid, lowest_ask-highest_bid)

	}

}

func generateSig(message, secret string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		return "", err
	}

	signature := hmac.New(sha256.New, key)
	_, err = signature.Write([]byte(message))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(signature.Sum(nil)), nil
}
func web_socket(ws_updates chan exchange.Message, semaphor chan bool) {
	var wsDialer ws.Dialer
	wsConn, _, err := wsDialer.Dial("wss://ws-feed.gdax.com", nil)
	if err != nil {
		println(err.Error())
	}

	method := "GET"
	url := "/users/self"
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	sign_me := fmt.Sprintf("%s%s%s", timestamp, method, url)
	var signature string
	signature, err = generateSig(sign_me, secret)
	subscribe := map[string]string{
		"type":       "subscribe",
		"product_id": product,
		"signature":  signature,
		"key":        key,
		"passphrase": passphrase,
		"timestamp":  timestamp,
	}
	//fmt.Printf("%+v\n", subscribe)
	if err := wsConn.WriteJSON(subscribe); err != nil {
		println(err.Error())
	}

	message := exchange.Message{}
	semaphor <- true
	for true {
		if err := wsConn.ReadJSON(&message); err != nil {
			println(err.Error())
			break
		}
		//println(message.Type, message.Side, message.Price, message.Size)
		ws_updates <- message
		select {
		case i1 := <-semaphor:
			if i1 == false {
				break
			}
		default:
			// noop
		}
	}
	wsConn.Close()

}
func Wait_for_updates(Ask, Bid *buntdb.DB, ws_updates chan exchange.Message, sequence int) {
	//var previous int
	for {
		message := <-ws_updates
		// disard messages received before the book download time
		if message.Sequence > sequence {
			if message.Side == "sell" {
				update_book(Ask, message)
			} else if message.Side == "buy" {
				update_book(Bid, message)
			}
		} else {
			//log.Print("Discarding some early packets...")
		}
	}
}
func update_book(Ask *buntdb.DB, message exchange.Message) {
	switch message.Type {
	case "received":
		// can ignore now
	case "open":
		var open InternalBook
		open.NumberOfOrders = 0
		open.OrderId = message.OrderId
		open.Price = message.Price
		open.Size = message.RemainingSize
		bytes, err := json.Marshal(open)
		if err != nil {
			log.Fatal(err)
		}

		err = Ask.Update(func(tx *buntdb.Tx) error {
			_, _, err := tx.Set(message.OrderId, string(bytes), nil)
			if err != nil {
				// If this happens, need to rebuild the whole order book!
				log.Println("Open error: ", err, message.Price, message.Side)
			}
			return nil
		})
		if err != nil {
			log.Println("Open: ", err)
		}

	case "done":
		// locate order on the book and delete it:
		// this may be more complicated?!
		Ask.Update(func(tx *buntdb.Tx) error {
			_, err := tx.Delete(message.OrderId)
			if err != nil {
				//log.Println("Done: ", err, message.Price, message.Size, message.Side)
			}
			return nil
		})
	case "match":
		var open InternalBook
		// locate order on the book and delete it:
		Ask.Update(func(tx *buntdb.Tx) error {
			value, err := tx.Get(message.MakerOrderId)
			if err != nil {
				log.Print("FUK! match, read:", err, message)
				if err != buntdb.ErrNotFound {
					return nil
				}
			}
			gjson.Unmarshal([]byte(value), &open)
			open.Size = open.Size - message.Size
			//log.Print("the open.Size is now:", open.Size, message.Size)
			//log.Print("the open.Price is now:", open.Price, message.Price)
			if open.Size <= 0.00005 { // fucking floats!
				_, err := tx.Delete(message.MakerOrderId)
				if err != nil {
					log.Print("match, delete: ", err)
					log.Print(open.Size, message.Size)
				}
			} else {
				bytes, err := json.Marshal(open)
				if err != nil {
					log.Fatal(err)
				}
				tx.Set(message.MakerOrderId, string(bytes), nil)
			}
			return nil
		})
	case "change":
		// locate order on the book and change it:
		// If the price is 0.0, then the message is a market order
		if message.Price != 0 {
			var open InternalBook
			err := Ask.Update(func(tx *buntdb.Tx) error {
				value, err := tx.Get(message.OrderId)
				if err != nil {
					log.Print("Change, read:", err)
					return nil
				}
				err = gjson.Unmarshal([]byte(value), &open)
				if err != nil {
					log.Print("gjson unmarshaling error!")
				}
				//fmt.Println("CHANGE: New vs. Old:", open.Size, message.OldSize)
				open.Size = message.NewSize
				bytes, err := json.Marshal(open)
				if err != nil {
					log.Fatal(err)
				}
				tx.Set(message.OrderId, string(bytes), nil)
				return nil
			})
			if err != nil {
				//log.Println("Change: ", err)
			}

		}

	default:

	}

}

func calculate_spread(Ask, Bid *buntdb.DB) float64 {
	//defer timeTrack(time.Now(), "spread calc.")
	//reset  spread?
	lowest_ask = 1e10
	highest_bid = 0
	err := Ask.View(func(tx *buntdb.Tx) error {
		tx.Ascend("Price", func(key, val string) bool {
			lowest_ask = gjson.Get(val, "Price").Num

			return false
		})
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
	err = Bid.View(func(tx *buntdb.Tx) error {
		tx.Descend("Price", func(key, val string) bool {
			highest_bid = gjson.Get(val, "Price").Num

			return false
		})
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
	if (lowest_ask - highest_bid) < 0 {
		fmt.Println(lowest_ask, highest_bid, lowest_ask-highest_bid)
		log.Fatal("Oh, shit, quitting!")
	}

	return lowest_ask - highest_bid
}
func get_recent_orders(DB *buntdb.DB, ask_flag bool) [][]string {
	result := [][]string{[]string{"Price", "Size"}}
	var max_results int = 9
	var counter int = 1
	DB.View(func(tx *buntdb.Tx) error {
		if ask_flag == true {
			tx.Ascend("Price", func(key, val string) bool {
				tmp_price := gjson.Get(val, "Price").Num
				tmp_size := gjson.Get(val, "Size").Num
				formated_string := []string{strconv.FormatFloat(tmp_price, 'f', 2, 64), strconv.FormatFloat(tmp_size, 'f', 2, 64)}
				result = append(result, formated_string)
				if counter < max_results {
					counter = counter + 1
					return true
				} else {
					return false
				}
			})
		} else {
			tx.Descend("Price", func(key, val string) bool {
				tmp_price := gjson.Get(val, "Price").Num
				tmp_size := gjson.Get(val, "Size").Num
				formated_string := []string{strconv.FormatFloat(tmp_price, 'f', 2, 64), strconv.FormatFloat(tmp_size, 'f', 2, 64)}
				result = append(result, formated_string)
				if counter < max_results {
					counter = counter + 1
					return true
				} else {
					return false
				}
			})
		}

		return nil
	})
	return result
}
func timeTrack(start time.Time, name string) {
	elapsed := time.Since(start)
	fmt.Printf("%s took %s \n", name, elapsed)
}
func Start_gui(Ask, Bid *buntdb.DB) {
	// Init
	if err := ui.Init(); err != nil {
		panic(err)
	}
	defer ui.Close()
	// init done!

	prices, volumes, labels := (func() ([]float64, []int, []string) {
		var price []float64
		var volume []int
		var label []string
		for _, val := range historic_rates {
			price = append(price, (val.Open+val.Close)/2)
			volume = append(volume, int(val.Volume*100000))
			label = append(label, val.Time.Format("05"))
		}
		return price, volume, label
	})()
	volume_spark := ui.NewSparkline()
	volume_spark.Data = volumes
	volume_spark.LineColor = ui.ColorGreen
	volume_box := ui.NewSparklines(volume_spark)
	volume_box.Height = 12
	volume_box.BorderLabel = "Volume"

	price_graph_box := ui.NewLineChart()
	price_graph_box.BorderLabel = "24 hour prices"
	price_graph_box.Data = prices
	price_graph_box.DataLabels = labels
	price_graph_box.Height = 12
	price_graph_box.AxesColor = ui.ColorWhite
	price_graph_box.LineColor = ui.ColorYellow | ui.AttrBold

	//
	ask_box := ui.NewTable()
	ask_box.Border = true
	ask_box.BorderLabel = "Asks"
	ask_box.Rows = [][]string{}
	ask_box.Height = 12
	ask_box.Separator = false
	//
	//
	bid_box := ui.NewTable()
	bid_box.Border = true
	bid_box.BorderLabel = "Bids"
	bid_box.Rows = [][]string{}
	bid_box.Height = 12
	bid_box.Separator = false
	//
	spread_box := ui.NewPar("0.01 (0.01) (0.02)")
	spread_box.Height = 3
	spread_box.BorderLabel = "Spread"

	instruction_box := ui.NewPar("Simple colored text with label. It [can be](fg-red) multilined with \\n or [break automatically](fg-red,fg-bold)")
	instruction_box.Height = 5
	instruction_box.BorderLabel = "Multiline"
	instruction_box.BorderFg = ui.ColorYellow

	latest_trade_box := ui.NewPar("Long text with label and it is auto trimmed.")
	latest_trade_box.Height = 3
	//spread_box.Y = 9
	latest_trade_box.BorderLabel = "Last trades"
	// build layout
	ui.Body.AddRows(
		ui.NewRow(
			ui.NewCol(6, 0, ask_box),
			ui.NewCol(6, 0, price_graph_box)),
		ui.NewRow(
			ui.NewCol(6, 0, spread_box),
			ui.NewCol(6, 0, latest_trade_box)),
		ui.NewRow(
			ui.NewCol(6, 0, bid_box),
			ui.NewCol(6, 0, volume_box)),
		ui.NewRow(ui.NewCol(12, 0, instruction_box)))

	// calculate layout
	ui.Body.Align()

	ui.Render(ui.Body)

	ui.Handle("/sys/kbd/q", func(ui.Event) {
		ui.StopLoop()
	})
	ui.Handle("/timer/1s", func(e ui.Event) {
		t := e.Data.(ui.EvtTimer)
		i := t.Count
		if i > 103 {
			ui.StopLoop()
			return
		}

		//volume_box.Lines[0].Data = spdata[:100+i]
		//price_graph_box.Data = sinps[2*i:]
		spread_box.Text = strconv.FormatFloat(calculate_spread(Ask, Bid), 'f', 2, 64)
		ask_rows := get_recent_orders(Ask, true)
		//log.Fatal(len(ask_rows), ask_rows)
		ask_box.Rows = ask_rows
		bid_rows := get_recent_orders(Bid, false)
		bid_box.Rows = bid_rows
		ui.Render(ui.Body)
	})

	ui.Handle("/sys/wnd/resize", func(e ui.Event) {
		ui.Body.Width = ui.TermWidth()
		ui.Body.Align()
		ui.Clear()
		ui.Render(ui.Body)
	})

	ui.Loop()
}
func reset_book(Ask, Bid *buntdb.DB) {
	// to be implemented to regularly reset the orderbook view in case of
	// a) connection (therefore packet) loss
	// b) general error
}
