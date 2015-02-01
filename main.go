package main

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/maueki/go_wiki/db"
	"log"
	"net/http"
	"strconv"
	"time"

	"code.google.com/p/go.crypto/bcrypt"

	"github.com/coopernurse/gorp"
	"github.com/flosch/pongo2"
	_ "github.com/flosch/pongo2-addons"
	_ "github.com/mattn/go-sqlite3"
	"github.com/zenazn/goji"
	"github.com/zenazn/goji/web"

	"github.com/bkaradzic/go-lz4"
	"github.com/gorilla/sessions"

	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

const SESSION_NAME = "go_wiki_session"

var store = sessions.NewCookieStore([]byte("something-very-secret")) // FIXME

type Page struct {
	Id                 int64 `db:"post_id"`
	Title              string
	Body               string
	LastModifiedUserId int64
	LastModifiedDate   time.Time
}

type History struct {
	Id             int64 `db:"history_id"`
	PageId         int64
	Title          []byte
	Body           []byte
	ModifiedUserId int64
	ModifiedDate   time.Time
}

type User struct {
	Id       bson.ObjectId `bson:"_id,omitempty"`
	Name     string
	Password []byte
}

type WikiDb struct {
	DbMap *gorp.DbMap
	Db    *mgo.Database
}

type LoginUser struct {
	Exist bool
	Name  string
}

func getUserId(r *http.Request) (int64, bool) {
	session, _ := store.Get(r, SESSION_NAME)

	if id, ok := session.Values["id"].(int64); ok {
		return id, true
	} else {
		return 0, false
	}
}

func encodeFromText(text string) ([]byte, error) {
	return lz4.Encode(nil, []byte(text))
}

func decodeFromBlob(data []byte) (string, error) {
	data, error := lz4.Decode(nil, data)
	return string(data), error
}

func (p *Page) createHistoryData() (*History, error) {
	title, err := encodeFromText(p.Title)
	if err != nil {
		return nil, err
	}

	body, err := encodeFromText(p.Body)
	if err != nil {
		return nil, err
	}

	history := History{}
	history.Title = title
	history.Body = body
	history.ModifiedUserId = p.LastModifiedUserId
	history.ModifiedDate = p.LastModifiedDate

	return &history, nil
}

func (p *Page) save(c web.C, r *http.Request) error {
	id, hasid := getUserId(r)
	if !hasid {
		return errors.New("failed to get user id.") // FIXME
	}

	p.LastModifiedUserId = id
	p.LastModifiedDate = time.Now()

	history, err := p.createHistoryData()
	if err != nil {
		return err
	}

	wikidb := getWikiDb(c)
	pOld := Page{}

	err = wikidb.DbMap.SelectOne(&pOld, "select * from page where post_id=?", p.Id)
	if err == sql.ErrNoRows {
		t, err := db.CreateTransaction(wikidb.DbMap)
		if err != nil {
			return err
		}

		t.Insert(p)

		history.PageId = p.Id
		t.Insert(history)

		return t.Subscribe()
	} else if err != nil {
		log.Fatalln(err)
	}

	p.Id = pOld.Id
	history.PageId = p.Id

	t, err := db.CreateTransaction(wikidb.DbMap)
	t.Update(p).Insert(history)
	return t.Subscribe()
}

func getPageFromDb(c web.C, pageId_str string) (*Page, error) {
	wikidb := getWikiDb(c)

	pageId, _ := strconv.ParseInt(pageId_str, 10, 64) // FIXME : err

	p := Page{}
	err := wikidb.DbMap.SelectOne(&p, "select * from page where post_id=?", pageId)
	if err != nil {
		fmt.Printf("getPageFromDb failed : %s\n", pageId_str)
		return nil, err
	}

	fmt.Printf("getPageFromDb success : %s\n", pageId_str)

	return &p, nil
}

func getUserById(db *mgo.Database, id bson.ObjectId) (*User, error) {
	user := User{}
	err := db.C("user").FindId(id).One(&user)
	return &user, err
}

func getLoginUserInfo(c web.C, w http.ResponseWriter, r *http.Request) (*LoginUser, error) {
	// check current user is success to login or not
	loginuser := LoginUser{false, ""}

	session, _ := store.Get(r, SESSION_NAME)
	id, ok := session.Values["id"]
	if ok {
		wikidb := getWikiDb(c)
		user, err := getUserById(wikidb.Db, bson.ObjectIdHex(id.(string)))
		if err == sql.ErrNoRows {
			delete(session.Values, "id")
			sessions.Save(r, w)
			fmt.Printf("not login\n")
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			loginuser.Exist = true
			loginuser.Name = user.Name
		}
	}

	return &loginuser, nil
}

func executeWriterFromFile(w http.ResponseWriter, path string, context *pongo2.Context) error {
	tpl := pongo2.Must(pongo2.FromFile(path))
	return tpl.ExecuteWriter(*context, w)
}

func createNewPageGetHandler(c web.C, w http.ResponseWriter, r *http.Request) {
	wikidb := getWikiDb(c)

	p := &Page{Title: "タイトル未設定", Body: ""}
	t, err := db.CreateTransaction(wikidb.DbMap)
	if err != nil {
		return
	}

	t.Insert(p)
	t.Subscribe()

	pageId_str := strconv.FormatInt(p.Id, 10)
	fmt.Printf("createNewPage pageId_str : %s\n", pageId_str)

	http.Redirect(w, r, "/wiki/"+pageId_str+"/edit", http.StatusFound)
}

func viewPageGetHandler(c web.C, w http.ResponseWriter, r *http.Request) {
	pageId_str := c.URLParams["pageId"]

	p, err := getPageFromDb(c, pageId_str)
	if p == nil || err != nil {
		// FIXME : redirect to top page or "NotFound" page
		http.Error(w, err.Error(), http.StatusNotFound)
	}

	loginuser, _ := getLoginUserInfo(c, w, r)

	err = executeWriterFromFile(w, "view/view.html",
		&pongo2.Context{"loginuser": loginuser, "page": p})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func editPageGetHandler(c web.C, w http.ResponseWriter, r *http.Request) {
	pageId_str := c.URLParams["pageId"]

	p, err := getPageFromDb(c, pageId_str)
	if err != nil {
		// FIXME : redirect to top page or "NotFound" page
		http.Error(w, err.Error(), http.StatusNotFound)
	}

	loginuser, _ := getLoginUserInfo(c, w, r)

	err = executeWriterFromFile(w, "view/edit.html",
		&pongo2.Context{"loginuser": loginuser, "page": p})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func savePagePostHandler(c web.C, w http.ResponseWriter, r *http.Request) {
	pageId_str := c.URLParams["pageId"]

	p, err := getPageFromDb(c, pageId_str)
	if err != nil {
		// FIXME : redirect to top page or "NotFound" page
		http.Error(w, err.Error(), http.StatusNotFound)
	}

	p.Body = r.FormValue("body")
	err = p.save(c, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/wiki/"+pageId_str, http.StatusFound)
}

func getWikiDb(c web.C) *WikiDb { return c.Env["wikidb"].(*WikiDb) }

func signupPageGetHandler(c web.C, w http.ResponseWriter, r *http.Request) {
	err := executeWriterFromFile(w, "view/signup.html", &pongo2.Context{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func HashPassword(password string) []byte {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalln("Hash password failed: %v", err)
		panic(err)
	}
	return hash
}

func signupPostHandler(c web.C, w http.ResponseWriter, r *http.Request) {
	wikidb := getWikiDb(c)
	name := r.FormValue("username")
	password := r.FormValue("password")

	user := &User{Name: name, Password: HashPassword(password)}
	err := wikidb.DbMap.Insert(user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	session, _ := store.Get(r, SESSION_NAME)
	session.Values["id"] = user.Id.Hex()
	sessions.Save(r, w)

	http.Redirect(w, r, "/wiki", http.StatusFound)
}

func loginPageGetHandler(c web.C, w http.ResponseWriter, r *http.Request) {
	err := executeWriterFromFile(w, "view/login.html", &pongo2.Context{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func loginPostHandler(c web.C, w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, SESSION_NAME)
	if session.IsNew {
		session.Options = &sessions.Options{
			Path:     "/",
			MaxAge:   86400 * 7, // 1week
			HttpOnly: true}
	}

	delete(session.Values, "id")
	sessions.Save(r, w)

	wikidb := getWikiDb(c)
	name := r.FormValue("username")
	password := r.FormValue("password")

	user := User{}
	err := wikidb.Db.C("user").Find(bson.M{"name": name}).One(&user)
	if err == mgo.ErrNotFound {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if err != nil {
		log.Fatalln(err)
	}

	err = bcrypt.CompareHashAndPassword(user.Password, []byte(password))
	if err != nil {
		// TODO: login failed
		fmt.Println("!!! login failed")
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	session.Values["id"] = user.Id.Hex()
	sessions.Save(r, w)

	http.Redirect(w, r, "/wiki", http.StatusFound)
}

func createTable(db *sql.DB) (*gorp.DbMap, error) {
	dbmap := &gorp.DbMap{Db: db, Dialect: gorp.SqliteDialect{}}
	pageTable := dbmap.AddTableWithName(Page{}, "page").SetKeys(true, "Id")
	pageTable.ColMap("Title").Rename("title")
	pageTable.ColMap("Body").Rename("body")
	pageTable.ColMap("LastModifiedUserId").Rename("lastuser")
	pageTable.ColMap("LastModifiedDate").Rename("lastdate")

	historyTable := dbmap.AddTableWithName(History{}, "history").SetKeys(true, "Id")
	historyTable.ColMap("PageId").Rename("pageid")
	historyTable.ColMap("Title").Rename("title")
	historyTable.ColMap("Body").Rename("body")
	historyTable.ColMap("ModifiedUserId").Rename("user")
	historyTable.ColMap("ModifiedDate").Rename("date")

	dbmap.DropTables()
	err := dbmap.CreateTables()
	if err != nil {
		return nil, err
	}

	return dbmap, err
}

func topPageGetHandler(c web.C, w http.ResponseWriter, r *http.Request) {
	loginuser, _ := getLoginUserInfo(c, w, r)
	err := executeWriterFromFile(w, "view/main.html", &pongo2.Context{"loginuser": loginuser})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func rootHandler(c web.C, w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/wiki", http.StatusFound)
}

func logoutPostHandler(c web.C, w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, SESSION_NAME)
	delete(session.Values, "id")
	sessions.Save(r, w)

	http.Redirect(w, r, "/wiki", http.StatusFound)
}

func markdownPostHandler(c web.C, w http.ResponseWriter, r *http.Request) {
	tpl, err := pongo2.FromString("{{text|markdown|sanitize}}")
	if err != nil {
		panic(err)
	}

	r.ParseForm()
	text := r.FormValue("text")
	err = tpl.ExecuteWriter(pongo2.Context{"text": text}, w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func includeDb(dbmap *gorp.DbMap, mongodb *mgo.Database) func(c *web.C, h http.Handler) http.Handler {
	wikidb := &WikiDb{
		DbMap: dbmap,
		Db:    mongodb,
	}

	return func(c *web.C, h http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			c.Env["wikidb"] = wikidb
			h.ServeHTTP(w, r)
		}
		return http.HandlerFunc(fn)
	}
}

func needLogin(c *web.C, h http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, SESSION_NAME)
		id, ok := session.Values["id"]
		if !ok {
			fmt.Printf("need login\n")
			http.Redirect(w, r, "/login", http.StatusFound)
		}

		wikidb := getWikiDb(*c)
		_, err := getUserById(wikidb.Db, bson.ObjectIdHex(id.(string)))
		if err == mgo.ErrNotFound {
			delete(session.Values, "id")
			sessions.Save(r, w)

			fmt.Printf("need login\n")
			http.Redirect(w, r, "/login", http.StatusFound)
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		h.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func addTestUser(db *mgo.Database) {
	db.C("user").RemoveAll(nil) // FIXME

	user := &User{
		Name: "test",
		Password: []byte("$2a$10$1KbzrHDRoPwZuHxWs1D6lOSLpcCRyPZXJ1Q7sPFbBf03DSc8y8n8K"),
	}

	err := db.C("user").Insert(user)
	if err != nil {
		log.Fatalln(err)
	}
}

func main() {
	ReadConfig()

	session, err := mgo.Dial("localhost")
	if err != nil {
		log.Fatalln(err)
	}
	defer session.Close()

	session.SetMode(mgo.Monotonic, true)

	mongodb := session.DB("gowiki")

	db, err := sql.Open("sqlite3", "./wiki.db")
	if err != nil {
		log.Fatalln(err)
	}

	dbmap, err := createTable(db)
	if err != nil {
		log.Fatalln(err)
	}
	defer dbmap.Db.Close()

	addTestUser(mongodb)

	m := web.New()
	m.Get("/signup", signupPageGetHandler)
	m.Get("/login", loginPageGetHandler)
	m.Post("/signup", signupPostHandler)
	m.Post("/login", loginPostHandler)
	m.Post("/logout", logoutPostHandler)
	m.Get("/wiki", topPageGetHandler)
	m.Get("/", rootHandler)

	loginUserActionMux := web.New()
	loginUserActionMux.Use(needLogin)
	loginUserActionMux.Use(includeDb(dbmap, mongodb))
	loginUserActionMux.Get("/action/createNewPage", createNewPageGetHandler)

	// Mux : create new page or show a page created already
	pageMux := web.New()
	pageMux.Use(needLogin)
	pageMux.Use(includeDb(dbmap, mongodb))
	pageMux.Get("/wiki/:pageId", viewPageGetHandler)
	pageMux.Get("/wiki/:pageId/edit", editPageGetHandler)
	pageMux.Post("/wiki/:pageId", savePagePostHandler)

	// Mux : convert Markdown to HTML which is send by Ajax
	mdMux := web.New()
	mdMux.Use(needLogin)
	mdMux.Use(includeDb(dbmap, mongodb))
	mdMux.Post("/markdown", markdownPostHandler)

	goji.Use(includeDb(dbmap, mongodb))
	goji.Get("/assets/*", http.FileServer(http.Dir(".")))
	goji.Handle("/wiki/*", pageMux)
	goji.Handle("/markdown", mdMux)
	goji.Handle("/action/*", loginUserActionMux)
	goji.Handle("/*", m)
	goji.Serve()
}
