package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

type ffprobeVideoFormat struct {
	Streams []struct {
		Width		int		`json:"width"`
		Height		int		`json:"height"`
	} `json:"streams"`
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	uploadLimit := int64(1 << 30) // 1 GB
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

	videoMetaData, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Couldn't get video", err)
		return
	}
	if videoMetaData.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You don't have permission to acces this resource", nil)
		return
	}

	fmt.Println("uploading video", videoID, "by user", userID)

	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit)
	err = r.ParseMultipartForm(uploadLimit)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "File too large or couldn't parse multipart form", err)
		return
	}
	file, fileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get file from form", err)
		return
	}
	defer file.Close()

	mediaTypeHeader := fileHeader.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(mediaTypeHeader)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't parse media type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid media type", fmt.Errorf("expected video/mp4, got %s", mediaType))
		return
	}
	newFile, err := os.CreateTemp("", "tubely-upload.mp4")

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create file", err)
		return
	}
	defer os.Remove(newFile.Name())
	defer newFile.Close()

	_, err = io.Copy(newFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy file", err)
		return
	}
	randomBytes := make([]byte, 32)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate random bytes", err)
		return
	}

	newFilePath, err := processVideoForFastStart(newFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video for fast start", err)
		return
	}
	newFile, err = os.Open(newFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed video file", err)
		return
	}
	defer os.Remove(newFilePath)
	defer newFile.Close()

	aspectRatio, err := getVideoAspectRatio(newFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}
	if aspectRatio == "16:9" {
		aspectRatio = "landscape"
	} else if aspectRatio == "9:16" {
		aspectRatio = "portrait"
	} else {
		aspectRatio = "other"
	}
	randomFileName := aspectRatio + "-" + base64.RawURLEncoding.EncodeToString(randomBytes) + ".mp4"
	

	if _, err := newFile.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't seek file", err)
		return
	}

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket: &cfg.s3Bucket,
		Key:    &randomFileName,
		Body:   newFile,
		ContentType: &mediaTypeHeader,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload file to S3", err)
		return
	}

	urlName := fmt.Sprintf("%s/%s", cfg.s3CfDistribution, randomFileName)
	videoMetaData.VideoURL = &urlName

	err = cfg.db.UpdateVideo(videoMetaData)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func getVideoAspectRatio(filepath string) (string, error) {
	command := fmt.Sprintf("ffprobe -v error -print_format json -show_streams %s", filepath)
	out, err := exec.Command("bash", "-c", command).Output()
	if err != nil {
		return "", fmt.Errorf("ffprobe command failed: %w", err)
	}
	var ffprobeVideo ffprobeVideoFormat

	if err := json.Unmarshal(out, &ffprobeVideo); err != nil {
		return "", fmt.Errorf("unmarshalling ffprobe output failed: %w", err)
	}
	if len(ffprobeVideo.Streams) == 0 {
		return "", fmt.Errorf("no video streams found in ffprobe output")
	}
	width := ffprobeVideo.Streams[0].Width
	height := ffprobeVideo.Streams[0].Height
	if height == 0 || width == 0 {
		return "", fmt.Errorf("invalid video dimensions: %dx%d", width, height)
	}
	gcd := func(a, b int) int {
		for b != 0 {
			a, b = b, a%b
		}
		return a
	}
	
	divisor := gcd(width, height)
	return fmt.Sprintf("%d:%d", width/divisor, height/divisor), nil	
}

func processVideoForFastStart(filepath string) (string, error) {
	outputFilePath := filepath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filepath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilePath)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg command failed: %w", err)
	}
	return outputFilePath, nil
}