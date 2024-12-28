package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"crypto/rand"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func processVideoForFastStart(filePath string) (string, error) {
	processingFile := filePath + ".processing"
	processCmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", processingFile)
	err := processCmd.Run()
	if err != nil {
		return "", err
	}
	return processingFile, nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var stdoutBuff bytes.Buffer
	cmd.Stdout = &stdoutBuff
	if err := cmd.Run(); err != nil {
		return "", err
	}
	type FFProbeOut struct {
		Streams []struct {
			DisplayAspectRatio string `json:"display_aspect_ratio"`
		} `json:"streams"`
	}
	decoder := json.NewDecoder(&stdoutBuff)
	var out FFProbeOut
	if err := decoder.Decode(&out); err != nil {
		return "", err
	}
	if len(out.Streams) < 1 {
		return "", errors.New("no steams to parse")
	}
	return out.Streams[0].DisplayAspectRatio, nil
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxUploadSize = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse form", err)
		return
	}
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You are not the owner of this video", err)
		return
	}

	inFile, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get the video", err)
		return
	}
	defer inFile.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "No Content-Type specified", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Only video/mp4 Content-Type supported", err)
		return
	}
	ext := "." + strings.Split(mediaType, "/")[1]

	tempVidFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}
	log.Printf("Saved video : %s\n", tempVidFile.Name())
	defer os.Remove(tempVidFile.Name())
	defer tempVidFile.Close()
	_, err = io.Copy(tempVidFile, inFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}
	_, err = tempVidFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}
	processedFilePath, err := processVideoForFastStart(tempVidFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video", err)
		return
	}
	defer os.Remove(processedFilePath)
	processedFileBodyBytes, err := os.ReadFile(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to read processed file", err)
		return
	}

	randBytes := make([]byte, 32)
	_, err = rand.Read(randBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}
	aspectRatio, err := getVideoAspectRatio(tempVidFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong while getting ratio", err)
		return
	}

	videoAspectRatioPrefix := ""
	switch aspectRatio {
	case "16:9":
		videoAspectRatioPrefix = "landscape"
	case "9:16":
		videoAspectRatioPrefix = "portrait"
	default:
		videoAspectRatioPrefix = "other"
	}

	objectKeyInBucket := videoAspectRatioPrefix + "/" + base64.RawURLEncoding.EncodeToString(randBytes) + ext

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &objectKeyInBucket,
		Body:        bytes.NewReader(processedFileBodyBytes),
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save video. Sorry.", err)
		return
	}
	videoURL := fmt.Sprintf("https://%s.cloudfront.net/%s", cfg.s3CfDistribution, objectKeyInBucket)
	video.VideoURL = &videoURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}
	respondWithJSON(w, http.StatusOK, video)
}
