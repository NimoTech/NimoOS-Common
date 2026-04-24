package storage

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	GB uint64 = 1024 * 1024 * 1024
)

type Partition struct {
	MountPoint string
	TotalSpace uint64 // bytes
	FreeSpace  uint64 // bytes
}

type NimoOSRecommendedPaths struct {
	Docker     string `json:"docker"`
	AppData    string `json:"appData"`
	UserData   string `json:"userData"`
	SystemData string `json:"systemData"`
}

// GetPhysicalPartitions parses /proc/mounts and uses syscall.Statfs to report physical disk partitions space.
func GetPhysicalPartitions() []Partition {
	file, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer file.Close()

	var partitions []Partition
	scanner := bufio.NewScanner(file)
	
	// Set of filesystems we consider as physical/persistent
	validFsMap := map[string]bool{
		"ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
		"fat32": true, "exfat": true, "ntfs": true, "vfat": true,
	}

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		
		device := fields[0]
		mountPoint := fields[1]
		fsType := fields[2]

		// Skip things that are not clearly physical block devices or root
		if !validFsMap[fsType] && !strings.HasPrefix(device, "/dev/") && device != "rootfs" {
			continue
		}

		// Avoid snap loopbacks or container mounts if any
		if strings.HasPrefix(device, "/dev/loop") {
			continue
		}

		var stat syscall.Statfs_t
		if err := syscall.Statfs(mountPoint, &stat); err != nil {
			continue
		}

		// Calculate sizes
		total := stat.Blocks * uint64(stat.Bsize)
		free := stat.Bavail * uint64(stat.Bsize)
		
		partitions = append(partitions, Partition{
			MountPoint: mountPoint,
			TotalSpace: total,
			FreeSpace:  free,
		})
	}
	
	return partitions
}

// RecommendStoragePaths calculates the optimal storage default paths dynamically
func RecommendStoragePaths() NimoOSRecommendedPaths {
	partitions := GetPhysicalPartitions()
	
	// Fallback single root if nothing could be parsed
	if len(partitions) == 0 {
		return NimoOSRecommendedPaths{
			Docker:     "/var/lib/docker",
			AppData:    "/home/nimo/nimoos/appdata",
			UserData:   "/home/nimo/nimoos/userdata",
			SystemData: "/var/lib/nimoos",
		}
	}

	var largestPartition *Partition
	var homePartition *Partition
	var varPartition *Partition
	
	maxTotalSpace := uint64(0)

	for i, p := range partitions {
		if p.TotalSpace > maxTotalSpace {
			maxTotalSpace = p.TotalSpace
			largestPartition = &partitions[i]
		}
		if p.MountPoint == "/home" {
			homePartition = &partitions[i]
		} else if p.MountPoint == "/var" {
			varPartition = &partitions[i]
		}
	}

	if largestPartition == nil {
		// shouldn't happen unless root is inaccessible
		largestPartition = &Partition{MountPoint: "/home/nimo"} 
	} else if largestPartition.MountPoint == "/" {
		// if root is the largest, prefer treating /home/nimo as the safe user space
		largestPartition.MountPoint = "/home/nimo"
	}

	paths := NimoOSRecommendedPaths{}

	// System Data
	paths.SystemData = "/var/lib/nimoos"

	// Docker Logic
	// If /var exists explicitly and has > 20GB, use it
	if varPartition != nil && varPartition.TotalSpace > 20*GB {
		paths.Docker = "/var/lib/docker"
	} else if largestPartition.MountPoint == "/home/nimo" || homePartition != nil {
		// otherwise, fallback to the largest or home-based approach
		base := "/home/nimo"
		paths.Docker = filepath.Join(base, "nimoos/docker")
	} else {
		paths.Docker = filepath.Join(largestPartition.MountPoint, "nimoos/docker")
	}

	// AppData and UserData - prioritize largest user facing space
	targetBase := largestPartition.MountPoint
	if targetBase == "/" || targetBase == "/home" {
		targetBase = "/home/nimo"
	}

	paths.AppData = filepath.Join(targetBase, "nimoos/appdata")
	paths.UserData = filepath.Join(targetBase, "nimoos/userdata")

	return paths
}
