package main
import("context";"fmt";"github.com/shindakun/goclaw/internal/runtime")
func main(){ m:=runtime.New("podman","localhost/goclaw-runner:latest",runtime.RuntimeCrun); fmt.Println("pre-launch:", m.EnsureRunner(context.Background(),1,"/var/folders/wc/jgqg__9s5v922t912qlllctr0000gn/T/tmp.0Zv8Hc97Bk/sessions/1")) }
