package main

import (
	"encoding/json"
	"os"
)

type Config struct {
	MemorySize      int    `json:"memory_size"`
	MessagePerHour  int    `json:"messages_per_hour"`
	MessagePerDay   int    `json:"messages_per_day"`
	TempBanDuration string `json:"temp_ban_duration"`
}

func loadConfig(filename string) (Config, error) {
	var config Config
	file, err := os.Open(filename)
	if err != nil {
		return config, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	err = decoder.Decode(&config)
	return config, err
}
