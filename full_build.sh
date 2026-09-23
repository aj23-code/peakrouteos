# 1. Compile the updated Go binary with the os.MkdirAll fix
go build -o peakroute main.go
# 2. Run your ISO build pipeline
sudo ./buildISO.sh
