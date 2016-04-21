package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/mail"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/gebi/scryptauth"
	"github.com/nytimes/gziphandler" //We might need some sort of license for this
	"golang.org/x/crypto/scrypt"
)

const (
	_version = 0.3
	port     = "8080"
)

var (
	//since opening and closing the database is considered
	//an expensive operation we keep this global to prevent
	//unneccesairy calls to the sql api
	db *DatabaseInterface

	_startTime = time.Now() //Last restart
	quit       = make(chan bool)
	secretKey  string
)

func main() {

	if runtime.GOOS == "windows" {
		c := exec.Command("cls")
		c.Stdout = os.Stdout
		c.Run()
	} else {
		c := exec.Command("clear")
		c.Stdout = os.Stdout
		c.Run()
	}

	//Setup back-end
	secretKey = randBase64String(128)
	db = connectToDatabase()
	go commandLineInterface(quit)
	go SessionCleaner(quit)
	fmt.Println("Server is running!")
	fmt.Println("Listening on PORT: " + port)

	//Setup client interface
	http.HandleFunc("/api/login", login)
	http.HandleFunc("/api/logout", logout)
	http.HandleFunc("/api/register", register)
	http.HandleFunc("/api/refreshtoken", refreshToken)
	http.HandleFunc("/api/profile/save", saveProfile)
	http.HandleFunc("/api/profile/get-edit", getProfileEdit)
	http.HandleFunc("/api/profile/get-view", getProfileView)

	http.HandleFunc("/api/upload/pdf", receiveUploadPDF)
	http.HandleFunc("/api/upload/profile-header", receiveUploadHeader)
	http.HandleFunc("/api/upload/profile-icon", receiveUploadIcon)

	//Setup gzip for everything
	fs := http.FileServer(http.Dir("www"))
	withoutGz := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.ServeHTTP(w, r)
	})
	withGz := gziphandler.GzipHandler(withoutGz)
	http.Handle("/", withGz)
	err := http.ListenAndServe(":"+port, nil)
	if err != nil {
		fmt.Println(err)
	}
}

// ---- Start of temp, for uploading files with profile ----
//TODO: Rewrite for our purpose
func receiveUploadHeader(w http.ResponseWriter, r *http.Request) {
	path, err := saveFile("img/profile-headers/", r)
	if err != nil {
		fmt.Println("Error:", err)
	}
	w.Write([]byte(path))
	fmt.Println("Upoaded header:", path)
}
func receiveUploadIcon(w http.ResponseWriter, r *http.Request) {
	path, err := saveFile("img/profile-icons/", r)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	w.Write([]byte(path))
	fmt.Println("Uploaded icon:", path)
}

func receiveUploadPDF(w http.ResponseWriter, r *http.Request) {
	path, err := saveFile("pdf/", r)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	w.Write([]byte(path))
	fmt.Println("Uploaded pdf:", path)
}

func saveFile(folder string, r *http.Request) (string, error) {
	r.ParseMultipartForm(32 << 20)
	file, handler, err := r.FormFile("file")
	if err != nil {
		return "", err
	}
	defer file.Close()
	path := folder + handler.Filename
	f, err := os.OpenFile("www/"+path, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		return "", err
	}
	defer f.Close()
	io.Copy(f, file)

	return path, nil
}

// ---- End of temp ----

//Checks the provided credentials and authenticates
//or denies the user.
func login(w http.ResponseWriter, r *http.Request) {
	var err error

	user, err := getClientBody(w, r)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}
	passString := user.Password

	//See if user is in database (if we're using one)
	if db != nil {
		user, err = db.LookupUser(user)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("User not found"))
			return
		}
	} else {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("No database present"))
		return
	}

	//Authenticate user and provide a jwt
	allowed := authenticatePassword(user, passString)
	if allowed {
		writeToken(w, r, user)
		db.InsertUserSession(user)
	} else {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("Incorrect email or password"))
	}
}

func refreshToken(w http.ResponseWriter, r *http.Request) {
	user, err := handleToken(w, r)
	if err != nil {
		return
	}
	user, err = db.GetUserSession(user)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(err.Error()))
		return
	}
	token, err := generateToken(user.UserID)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Unable to provide webtoken"))
		return
	}
	user.Token = token
	err = db.UpdateUserSession(user)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Println(err)
		w.Write([]byte("Unable to update token value in database"))
		return
	}
	writeToken(w, r, user)
}

//Removes the active session for the user in database which will
//make the rest of the code treat the user as not logged in
func logout(w http.ResponseWriter, r *http.Request) {
	user, err := handleToken(w, r)
	if err != nil {
		return
	}
	user, err = db.GetUserSession(user)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		w.Write([]byte("Not logged in"))
		return
	}
	err = db.RemoveUserSession(user.Session)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		w.Write([]byte("Not currently logged in"))
		return
	}

}

//Encrypts the users password and registers it in the database
func register(w http.ResponseWriter, r *http.Request) {
	user, err := getClientBody(w, r)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}
	user.UserID = randBase64String(64)
	user.Salt = randBase64String(128)

	//Since the code will be run by a raspberry pi, 65536 is the best
	//we can do when it comes to cost for our key. Should be increased
	//to 1048576 (1 << 20) when migrating to a more high end system.
	passwordHash, _ := scrypt.Key([]byte(user.Password), []byte(user.Salt), (1 << 16), 8, 1, 128)
	passwordHash64 := scryptauth.EncodeBase64((1 << 16), []byte(passwordHash), []byte(user.Salt))
	user.Password = string(passwordHash64)

	if db != nil {
		err := db.AddUser(user)
		if err != nil {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte("User already registered"))
			return
		}
		writeToken(w, r, user)
		db.InsertUserSession(user)
	}
}

//Generates a KDF from the provided password and user salt and compares them
//Returns true/false if hashes match
func authenticatePassword(user *User, password string) bool {
	passwordHash, _ := scrypt.Key([]byte(password), []byte(user.Salt), (1 << 16), 8, 1, 128)
	passwordHash64 := scryptauth.EncodeBase64((1 << 16), []byte(passwordHash), []byte(user.Salt))
	return (string(passwordHash64) == user.Password)

}

//Returns a profile to the client
func getProfileView(w http.ResponseWriter, r *http.Request) {
	user, err := getClientBody(w, r)
	if err != nil {
		fmt.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}
	writeUserContentToClient(w, r, user)
}

//Validates token and returns a profile to client for edit
func getProfileEdit(w http.ResponseWriter, r *http.Request) {
	if db == nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("No database associated"))
		return
	}

	user, err := handleToken(w, r) //feedback to client happens inside function
	if err != nil {
		return
	}
	writeUserContentToClient(w, r, user)
}

//Writes UserContent from database to client
func writeUserContentToClient(w http.ResponseWriter, r *http.Request, user *User) {
	userContent := new(UserContents)
	userContent, err := db.GetUserContents(user.UserID, userContent)
	if err != nil {
		fmt.Println(err)
		w.WriteHeader(http.StatusNoContent)
		w.Write([]byte("No content for the specified user"))
		return
	}

	JSON, err := json.Marshal(userContent)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Unable to send content"))
		return
	}
	w.WriteHeader(http.StatusAccepted)
	w.Write(JSON)
}

//Serves the profile into database
func saveProfile(w http.ResponseWriter, r *http.Request) {
	if db == nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("No database associated"))
		return
	}

	_, err := handleToken(w, r) //feedback to client happens inside function
	if err != nil {
		return
	}
	body, err := ioutil.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Could not read content"))
		return
	}
	userContent := new(UserContents)

	//Parse json into UserContent struct
	err = json.Unmarshal(body, userContent)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Unexpected end of json-input"))
		return
	}
	user := new(User)
	user.Token = strings.Split(r.Header.Get("Authorization"), " ")[1]
	_, err = db.GetUserSession(user)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("No active session"))
		return
	}
	err = db.UpdateUserContent(user.UserID, userContent)
	if err != nil {
		w.WriteHeader(http.StatusNotAcceptable)
		fmt.Println(err)
		w.Write([]byte(err.Error()))
		return
	}
}

//Uses the jwt-library and the secretKey to generate a signed jwt
func generateToken(userID string) (string, error) {
	token := jwt.New(jwt.SigningMethodHS256)
	token.Claims["uid"] = userID
	token.Claims["exp"] = time.Now().Add(time.Minute * 5).Unix()
	tokenString, err := token.SignedString([]byte(secretKey))
	if err != nil {
		fmt.Println(err)
		return "", err
	}
	return tokenString, nil
}

//Reads n crypto random bytes and return them as a base64 encoded string
func randBase64String(n int) string {
	bytes := make([]byte, n)
	rand.Read(bytes)

	return base64.URLEncoding.EncodeToString(bytes)
}

//Takes the request from the client and parses the json
//inside into a user struct which is being returned
func getClientBody(w http.ResponseWriter, r *http.Request) (*User, error) {
	//Read json from client
	body, err := ioutil.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		return nil, err
	}
	user := new(User)

	//Parse json into User struct
	err = json.Unmarshal(body, user)
	if err != nil {
		return nil, err
	}
	err = validateEmail(user.Email)
	if err != nil {
		user.Email = ""
	}
	return user, nil
}

//Validates an email provided by the user
func validateEmail(email string) error {
	_, err := mail.ParseAddress(email)
	if err != nil {
		return err
	} else if strings.Contains(email, "'") {
		return errors.New("Email cannot contain '")
	}
	return nil
}

//handleToken takes care of reading and validating the token provided by the client
//returns a user containing token and user session
func handleToken(w http.ResponseWriter, r *http.Request) (*User, error) {
	user := new(User)
	providedTokens := strings.Split(r.Header.Get("Authorization"), " ")
	if len(providedTokens) != 2 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Invlaid number of tokens provided"))
		return nil, errors.New("Invlaid number of tokens provided")
	}
	user.Token = providedTokens[1]

	user, err := db.GetUserSession(user)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(err.Error()))
		return nil, err
	}

	valid, _ := validateToken(user)
	if !valid {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("Invalid web token"))
		return nil, errors.New("Invalid web token")
	}
	return user, nil
}

//Validates a token
func validateToken(user *User) (bool, *jwt.Token) {
	token, err := jwt.Parse(user.Session.SessionKey, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secretKey), nil
	})
	if err != nil {
		fmt.Println(err)
		return false, nil
	}
	if token.Claims["uid"] != user.UserID {
		token.Valid = false
	} else if token.Claims["exp"].(float64) <= float64(time.Now().Unix()) {
		token.Valid = false
	}

	if token.Valid {
		return true, token
	}
	fmt.Println(err)
	return false, token
}

//Generates a token and writes it to the client
func writeToken(w http.ResponseWriter, r *http.Request, user *User) {
	token, err := generateToken(user.UserID)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Unable to provide web token"))
		return
	}
	user.Token = token

	JSON, err := json.Marshal(Response{token})
	if err != nil {
		fmt.Println(err)
	}
	w.WriteHeader(http.StatusAccepted)
	w.Write(JSON)
}

//Tries to open a connection to the database
//On success: global value db is assigned a DatabaseInterface-struct
//On failure: global value db is assigned nil
func connectToDatabase() *DatabaseInterface {
	conf, err := os.Open(".db_cnf")
	if err != nil {
		db = nil
		fmt.Println("No database config file detected")
		fmt.Println("Continuing without database")
		return nil
	}
	defer conf.Close()

	db := new(DatabaseInterface)
	db.SetConfigurations(conf)
	err = db.OpenConnection()
	if err != nil {
		fmt.Println("Failed to connect to database with error:")
		fmt.Println(err)
		fmt.Println("Continuing without database")
		return nil
	}
	fmt.Println("Successfully connected to database")
	return db
}

//Takes care of closing operations
func closeServer() {
	fmt.Println("Bye!")
	if db != nil {
		db.CloseConnection()
	}
	close(quit)
	os.Exit(0)
}
