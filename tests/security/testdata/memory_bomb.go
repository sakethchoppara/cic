package main

func main() {
	var chunks [][]byte
	for {
		b := make([]byte, 10<<20)
		for i := range b {
			b[i] = 1
		}
		chunks = append(chunks, b)
	}
}
