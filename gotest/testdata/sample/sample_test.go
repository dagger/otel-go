package sample_test

import (
	"testing"
	"time"
)

func TestPass(t *testing.T) {
	time.Sleep(10 * time.Millisecond)
	t.Log("this test passes")
}

func TestFail(t *testing.T) {
	time.Sleep(10 * time.Millisecond)
	t.Error("something went wrong")
}

func TestSkip(t *testing.T) {
	t.Skip("not implemented yet")
}

func TestSub(t *testing.T) {
	t.Run("level1", func(t *testing.T) {
		t.Log("in level1")
		t.Run("level2", func(t *testing.T) {
			time.Sleep(5 * time.Millisecond)
			t.Log("in level2")
		})
	})
}

func TestParallel0(t *testing.T) {
	t.Parallel()
	time.Sleep(2 * time.Second)
}

func TestParallel1(t *testing.T) {
	t.Parallel()
	time.Sleep(500 * time.Millisecond)
	t.Run("a", func(t *testing.T) {
		t.Parallel()
		time.Sleep(1 * time.Second)
		t.Log("parallel a done")
	})
	t.Run("b", func(t *testing.T) {
		t.Parallel()
		time.Sleep(2 * time.Second)
		t.Log("parallel b done")
	})
}

func TestParallel2(t *testing.T) {
	t.Parallel()
	time.Sleep(2 * time.Second)
}
