env "local" {
  src = "file://db/schema.sql"
  dev = "sqlite://dev?mode=memory"

  migration {
    dir = "file://db/migrations"
  }
}
