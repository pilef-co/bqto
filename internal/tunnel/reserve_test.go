package tunnel

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReservePortSkipsAPortAnotherSessionReserved(t *testing.T) {
	dir := t.TempDir()
	start, err := InnerPort()
	if err != nil {
		t.Fatal(err)
	}
	first, releaseFirst, err := ReservePort(dir, start)
	if err != nil {
		t.Fatal(err)
	}
	second, releaseSecond, err := ReservePort(dir, start)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()
	if first == second {
		t.Fatalf("two reservations got the same port %d", first)
	}
	if ln, err := Listen(first); err != nil {
		t.Errorf("a reserved port must stay free for kafka-proxy to bind: %v", err)
	} else {
		ln.Close()
	}
	releaseFirst()
	again, releaseAgain, err := ReservePort(dir, start)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseAgain()
	if again != first {
		t.Errorf("after the release got %d, want %d back", again, first)
	}
}

func TestReservePortIsFreedWhenTheSessionDies(t *testing.T) {
	if os.Getenv("BQTO_RESERVE_HELPER") == "1" {
		want, _ := strconv.Atoi(os.Getenv("BQTO_RESERVE_FROM"))
		p, _, err := ReservePort(os.Getenv("BQTO_RESERVE_DIR"), want)
		if err != nil {
			os.Exit(2)
		}
		_, _ = os.Stdout.WriteString(strconv.Itoa(p) + "\n")
		time.Sleep(time.Minute)
		return
	}
	dir := t.TempDir()
	start, err := InnerPort()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestReservePortIsFreedWhenTheSessionDies$")
	child.Env = append(os.Environ(), "BQTO_RESERVE_HELPER=1", "BQTO_RESERVE_FROM="+strconv.Itoa(start), "BQTO_RESERVE_DIR="+dir)
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	line, _ := bufio.NewReader(out).ReadString('\n')
	held, _ := strconv.Atoi(strings.TrimSpace(line))
	if held == 0 {
		t.Fatal("the helper reserved no port")
	}
	if p, release, err := ReservePort(dir, held); err == nil {
		release()
		if p == held {
			t.Fatalf("port %d reserved twice while its session ran", held)
		}
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	p, release, err := ReservePort(dir, held)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if p != held {
		t.Errorf("after the session was killed got %d, want %d back", p, held)
	}
}
