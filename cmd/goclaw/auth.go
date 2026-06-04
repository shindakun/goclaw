package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/shindakun/goclaw/internal/config"
	"github.com/shindakun/goclaw/internal/credstore"
	"github.com/shindakun/goclaw/internal/db"
)

// runAuth handles `goclaw auth <subcommand>` - managing the encrypted credential
// store the bundled proxy reads (brief §8). Operates directly on goclaw.db, so
// it works whether or not the host is running.
func runAuth(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage:\n" +
			"  goclaw auth add <name> <target-api-url> <token>   store a credential\n" +
			"  goclaw auth list                                  list stored credentials\n" +
			"  goclaw auth delete <id>                           delete by id (from list)")
	}

	store, closeDB, err := openCredStore()
	if err != nil {
		return err
	}
	defer closeDB()

	switch args[0] {
	case "add":
		return authAdd(store, args[1:])
	case "list":
		return authList(store)
	case "delete", "rm":
		return authDelete(store, args[1:])
	default:
		return fmt.Errorf("unknown auth subcommand %q (try: add, list, delete)", args[0])
	}
}

// openCredStore loads config, opens the central DB (applying migrations), and
// builds a credstore. The caller must call the returned close func.
func openCredStore() (*credstore.Store, func(), error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	d, err := db.Open(cfg.CentralDBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open central db: %w", err)
	}
	store := credstore.New(d.DB, cfg.SecretEncryptionKey)
	return store, func() { _ = d.Close() }, nil
}

func authAdd(store *credstore.Store, args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: goclaw auth add <name> <target-api-url> <token>")
	}
	if !store.HasKey() {
		return fmt.Errorf("GOCLAW_SECRET_ENCRYPTION_KEY is unset or not a 32-byte base64 key.\n" +
			"Generate one with:  head -c 32 /dev/urandom | base64\n" +
			"then set it in your environment/.env before storing credentials")
	}
	name, targetURL, token := args[0], args[1], args[2]
	id, err := store.Add(name, targetURL, token)
	if err != nil {
		return err
	}
	fmt.Printf("Stored credential %q for %s\n", name, targetURL)
	fmt.Printf("  id: %s\n", id)
	fmt.Println("  The proxy will inject this token for requests to that host.")
	fmt.Println("  The raw token never enters the agent container.")
	return nil
}

func authList(store *credstore.Store) error {
	creds, err := store.List()
	if err != nil {
		return err
	}
	if len(creds) == 0 {
		fmt.Println("No credentials stored. Add one with:")
		fmt.Println("  goclaw auth add <name> <target-api-url> <token>")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tTARGET\tTOKEN")
	for _, c := range creds {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.ID, c.Name, c.TargetURL, c.Preview)
	}
	return w.Flush()
}

func authDelete(store *credstore.Store, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: goclaw auth delete <id>   (see ids with: goclaw auth list)")
	}
	gone, err := store.Delete(args[0])
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("no credential with id %q (see: goclaw auth list)", args[0])
	}
	fmt.Printf("Deleted credential %s\n", args[0])
	return nil
}
