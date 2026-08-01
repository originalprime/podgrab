package db

import (
	"fmt"
	"log"
	"os"
	"path"

	"gorm.io/driver/sqlite"

	"gorm.io/gorm"
)

//DB is
var DB *gorm.DB

//Init is used to Initialize Database
func Init() (*gorm.DB, error) {
	// github.com/mattn/go-sqlite3
	configPath := os.Getenv("CONFIG")
	dbPath := path.Join(configPath, "podgrab.db")
	log.Println(dbPath)
	// _journal_mode=WAL: readers no longer block writers (and vice versa), which
	// matters once background downloads and web requests are hitting the DB at
	// the same time. _busy_timeout: have SQLite retry briefly on a lock instead
	// of immediately returning "database is locked".
	dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=5000"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		fmt.Println("db err: ", err)
		return nil, err
	}

	localDB, _ := db.DB()
	localDB.SetMaxIdleConns(10)
	localDB.SetMaxOpenConns(10)
	//db.LogMode(true)
	DB = db
	return DB, nil
}

//Migrate Database
func Migrate() {
	DB.AutoMigrate(&Podcast{}, &PodcastItem{}, &Setting{}, &Migration{}, &JobLock{}, &Tag{})
	RunMigrations()
}

// Using this function to get a connection, you can create your connection pool here.
func GetDB() *gorm.DB {
	return DB
}
