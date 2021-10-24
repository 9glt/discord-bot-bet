package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"

	"go.etcd.io/bbolt"
)

var (
	Token         string
	MinimumPoints uint64
)

func init() {

	flag.StringVar(&Token, "t", "", "Bot Token")
	flag.Uint64Var(&MinimumPoints, "p", 1000000, "Minimum points to exchange")
	flag.Parse()
	rand.Seed(time.Now().UnixNano())
}

var (
	gdb *bbolt.DB
)

func ghash(g string, u string) []byte {
	return []byte(g + "-" + u)
}

type UserData struct {
	ID       string
	GID      string
	Points   uint64
	TimeLeft int64
	Bets     uint64
	Wins     uint64
	Loses    uint64
}

type By func(p1, p2 *UserData) bool

func (by By) Sort(users []UserData) {
	ps := &UserDatas{
		Users: users,
		by:    by,
	}
	sort.Sort(ps)
}

type UserDatas struct {
	Users []UserData
	by    func(p1, p2 *UserData) bool
}

func (s *UserDatas) Len() int {
	return len(s.Users)
}

// Swap is part of sort.Interface.
func (s *UserDatas) Swap(i, j int) {
	s.Users[i], s.Users[j] = s.Users[j], s.Users[i]
}

// Less is part of sort.Interface. It is implemented by calling the "by" closure in the sorter.
func (s *UserDatas) Less(i, j int) bool {
	// return s.by(&s.Users[i], &s.Users[j])
	return s.by(&s.Users[i], &s.Users[j])
}

func resetGuilds() {
	gdb.Update(func(tx *bbolt.Tx) error {
		for k, _ := tx.Bucket([]byte("locks")).Cursor().First(); k != nil; k, _ = tx.Bucket([]byte("locks")).Cursor().Next() {
			tx.Bucket([]byte("locks")).Put(k, []byte("0"))
		}
		return nil
	})
}

func setUserData(h []byte, ud UserData) {
	b, _ := json.Marshal(ud)
	err := gdb.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("main")).Put(h, b)
	})
	if err != nil {
		log.Printf("%v", err)
	}
}

func getUserData(h []byte) *UserData {
	var ud UserData
	err := gdb.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket([]byte("main")).Get(h)
		return json.Unmarshal(v, &ud)
	})
	if err != nil {
		log.Printf("%v", err)
	}
	return &ud
}

func getGuildUsers(g string, by By) []UserData {
	var ud []UserData
	gdb.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket([]byte("main")).Cursor()
		for k, v := c.Seek([]byte(g + "-")); k != nil && bytes.HasPrefix(k, []byte(g+"-")); k, v = c.Next() {
			var u UserData
			json.Unmarshal(v, &u)
			if u.ID == "" {
				continue
			}
			ud = append(ud, u)
		}
		return nil
	})

	By(by).Sort(ud)
	return ud
}

func lockedGuildGame(g string) bool {
	var r bool
	gdb.Update(func(tx *bbolt.Tx) error {
		v := tx.Bucket([]byte("locks")).Get([]byte(g))
		r = bytes.Compare(v, []byte("1")) == 0
		return nil
	})
	return r
}

func lockGuildGame(g string) bool {
	var r bool
	gdb.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("locks")).Put([]byte(g), []byte("1"))
	})
	return r
}

func unlockGuildGame(g string) bool {
	var r bool
	gdb.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("locks")).Put([]byte(g), []byte("0"))
	})
	return r
}

func main() {
	var err error
	gdb, err = bbolt.Open("./storage.db", 0755, nil)
	if err != nil {
		panic(err)
	}
	gdb.Update(func(tx *bbolt.Tx) error {
		tx.CreateBucketIfNotExists([]byte("main"))
		tx.CreateBucketIfNotExists([]byte("locks"))
		return nil
	})

	resetGuilds()

	dg, err := discordgo.New("Bot " + Token)
	if err != nil {
		fmt.Println("error creating Discord session,", err)
		return
	}

	dg.AddHandler(messageCreate)

	dg.Identify.Intents = discordgo.IntentsGuildMessages

	err = dg.Open()
	if err != nil {
		fmt.Println("error opening connection,", err)
		return
	}

	// Wait here until CTRL-C or other term signal is received.
	fmt.Println("Bot is now running.  Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc

	// Cleanly close down the Discord session.
	dg.Close()
}

type MyUser struct {
	ID    string
	Name  string
	Roles []string
}

var space = regexp.MustCompile(`\s+`)

var circles = []string{":green_circle:", ":blue_circle:", ":red_circle:", ":yellow_circle:", ":orange_circle:", ":purple_circle:", ":white_circle:"}

var hourly = make(map[string]int64)
var balances = make(map[string]int)

var games = make(map[string]bool)
var inGame bool

var bhlock = &sync.RWMutex{}

var RegexUserPatternID *regexp.Regexp = regexp.MustCompile(fmt.Sprintf(`^<@!(\d{%d,})>$`, 16))
var RegexUserPatternID1 *regexp.Regexp = regexp.MustCompile(fmt.Sprintf(`^<@(\d{%d,})>$`, 16))

var winners = []string{":crown:", ":second_place:", ":third_place:" /*":three:", */, ":four:", ":five:", ":six:", ":seven:", ":eight:", ":nine:", ":keycap_ten:"}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}

	contents := space.ReplaceAllString(m.Content, " ")
	if len(contents) <= 0 {
		return
	}
	parts := strings.Split(contents, " ")
	cmd := parts[0]
	if cmd[0] != ';' {
		return
	}

	if cmd == ";help" {
		s.ChannelMessageSend(m.ChannelID, ";hourly - kas valandinė atsitiktinė dovana\n;bet [amount] - pradėk statymą!\n;give [user] [amount] - perleisti kitam vartotojui taškų kiekį\n;top - žaidėjū top 10 statistika.\n;bal - žaidėjo balansas.")
	}

	if cmd == ";give" {
		if len(parts) != 3 {
			return
		}

		if RegexUserPatternID.MatchString(parts[1]) || RegexUserPatternID1.MatchString(parts[1]) {
			usr, err := s.User(m.Mentions[0].ID)
			if usr.ID == m.Author.ID {
				s.ChannelMessageSend(m.ChannelID, "noop")
				return
			}
			if err != nil {
				log.Printf("%v", err)
			}

			ud1 := getUserData(ghash(m.GuildID, m.Author.ID))
			ud1.ID = m.Author.ID
			ud2 := getUserData(ghash(m.GuildID, usr.ID))
			ud2.ID = usr.ID

			p2, _ := strconv.Atoi(parts[2])
			if p2 < 0 {
				return
			}
			if ud1.Points >= uint64(p2) {
				bhlock.Lock()

				ud1.Points -= uint64(p2)
				ud2.Points += uint64(p2)

				setUserData(ghash(m.GuildID, m.Author.ID), *ud1)
				setUserData(ghash(m.GuildID, usr.ID), *ud2)

				bal1 := ud2.Points
				bhlock.Unlock()
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("%s padovanojo %s taškus %s. %s dabar turi %d taškus.", m.Author.Mention(), parts[2], usr.Mention(), usr.Mention(), bal1))
			} else {
				bhlock.RLock()
				ud1 := getUserData(ghash(m.GuildID, m.Author.ID))
				bal1 := ud1.Points
				bhlock.RUnlock()
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("%s, Jūs turite tik %d taškus.", m.Author.Mention(), bal1))
			}
		}
		return
	}

	if cmd == ";exchange" {
		if len(parts) != 3 {
			return
		}
		if len(m.Mentions) == 0 {
			return
		}
		usr, err := s.User(m.Mentions[0].ID)
		if usr.ID == m.Author.ID {
			s.ChannelMessageSend(m.ChannelID, "noop")
			return
		}
		if err != nil {
			log.Printf("%v", err)
		}

		p2, err := strconv.Atoi(parts[2])
		if p2 < 0 || err != nil {
			if err != nil {
				log.Printf("%v", err)
			}
			return
		}
		if p2 < int(MinimumPoints) {
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Iškeisti galima mažiausiai %d taškų", MinimumPoints))
			return
		}

		ud1 := getUserData(ghash(m.GuildID, m.Author.ID))

		if ud1.Points < uint64(p2) {
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Neturite %d taškų", p2))
			return
		}

		ud1.Points -= uint64(p2)

		ch, err := s.UserChannelCreate(usr.ID)
		if err != nil {
			log.Printf("%v", err)
			return
		}
		setUserData(ghash(m.GuildID, m.Author.ID), *ud1)

		s.ChannelMessageSend(ch.ID, fmt.Sprintf(m.Author.String()+" nori keisti %d taškus į prizą", p2))
		s.ChannelMessageSend(m.ChannelID, "Užklausimas sėkmingai perduotas "+usr.Mention())

	}

	if cmd == ";bal" {
		ud1 := getUserData(ghash(m.GuildID, m.Author.ID))
		bal1 := ud1.Points
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("%s balansas %d taškai.", m.Author.Mention(), bal1))
		return
	}

	if cmd == ";top" {
		users := getGuildUsers(m.GuildID, func(u1, u2 *UserData) bool {
			return u2.Points < u1.Points
		})
		rt := ""
		k := 0
		for _, user := range users {
			if k >= 10 {
				break
			}
			u, err := s.User(user.ID)
			if err == nil {
				rt += winners[k] + " "
				rt += fmt.Sprintf("%s turi %d taškųm %d kartų laimėjo, %d  kartų pralošė.\n", u.Username, user.Points, user.Wins, user.Loses)
			}
			k++
		}

		s.ChannelMessageSend(m.ChannelID, rt)
		return
	}

	if cmd == ";hourly" {
		bhlock.Lock()
		defer bhlock.Unlock()
		ud1 := getUserData(ghash(m.GuildID, m.Author.ID))
		ud1.ID = m.Author.ID

		const ttl int64 = 3600

		if time.Now().Unix()-ud1.TimeLeft > ttl {
			ud1.TimeLeft = time.Now().Unix()
			am := uint64(rand.Intn(300-100+1) + 100)
			ud1.Points += am
			setUserData(ghash(m.GuildID, m.Author.ID), *ud1)

			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("%s gavai dovanų %d taškus", m.Author.Mention(), am))
		} else {
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("%s iki dovanos liko laukti %d sekundžių. Pabandykite vėliau.", m.Author.Mention(), ttl-(time.Now().Unix()-ud1.TimeLeft)))
		}
		return
	}

	if cmd == ";bet" {
		if len(parts) != 2 {
			return
		}
		amount, _ := strconv.Atoi(strings.ReplaceAll(parts[1], "k", "000"))
		if amount <= 0 {
			return
		}

		bhlock.RLock()

		if lockedGuildGame(m.GuildID) {
			s.ChannelMessageSend(m.ChannelID, "Žaidimas vyksta.... "+m.Author.Mention()+" palaukite kol pasibaigs.")
			bhlock.RUnlock()
			return
		}

		lockGuildGame(m.GuildID)
		defer func() {
			unlockGuildGame(m.GuildID)
		}()

		ud1 := getUserData(ghash(m.GuildID, m.Author.ID))
		ud1.ID = m.Author.ID
		ud1.Bets++
		if ud1.Points < uint64(amount) {
			bal := ud1.Points
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("%s negalite lažintis iš %d sumos, nes turite tik %d taškų.", m.Author.Mention(), amount, bal))
			bhlock.RUnlock()
			return
		}
		bhlock.RUnlock()

		user, _ := s.User(m.Author.ID)

		bhlock.Lock()
		if ud1.Points-uint64(amount) >= 0 {
			ud1.Points -= uint64(amount)
		}

		bhlock.Unlock()

		_ = amount

		ll := []string{}

		var mp = make(map[int]int)

		for i := 0; i < 3; i++ {
			n := rand.Intn(len(circles))
			_, ok := mp[n]
			if !ok {
				mp[n] = 1
			} else {
				mp[n]++
			}
			ll = append(ll, circles[n])
		}

		var winner = false
		var doublewinner = false
		for _, v := range mp {
			if v == 3 {
				doublewinner = true
			} else if v == 2 {
				winner = true
			}
		}

		if doublewinner {
			ud1.Points += uint64(amount) * 8
			ud1.Wins++
		} else if winner {
			ud1.Points += uint64(amount) * 3
			ud1.Wins++
		} else {
			ud1.Loses++
		}
		setUserData(ghash(m.GuildID, m.Author.ID), *ud1)

		mm := []string{}

		dm, _ := s.ChannelMessageSend(m.ChannelID, ll[0])
		mm = append(mm, ll[0])
		time.Sleep(time.Second)
		for i := 1; i < 3; i++ {
			mm = append(mm, ll[i])
			_, err := s.ChannelMessageEdit(m.ChannelID, dm.ID, strings.Join(mm, " "))
			if err != nil {
				log.Printf("%v", err)
			}
			time.Sleep(time.Second)
		}

		s.ChannelMessageDelete(m.ChannelID, dm.ID)

		if doublewinner {
			bal1 := ud1.Points
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Laimėjote, %s : %d. Jūsų balansas: %d", user.Mention(), uint64(amount)*8, bal1))
		} else if winner {
			bal1 := ud1.Points
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Laimėjote %s : %d. Jūsų balansas: %d", user.Mention(), uint64(amount)*3, bal1))
		} else {
			s.ChannelMessageSend(m.ChannelID, user.Mention()+", gaila, bet šį kart nepaėjo. Linkime sėkmės kitą kartą. Lažinkis atsakingai!")

		}
		bhlock.Lock()
		inGame = false
		bhlock.Unlock()
		return
	}
}
