package qemu

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// writeOVA produces an OVA at ovaPath that VirtualBox / VMware
// Workstation accept via File -> Import Appliance. The OVA is
// an uncompressed tar containing two members, in this order:
//
//	<name>.ovf   - XML descriptor (must come first per OVF 1.0
//	               streaming-import contract)
//	<name>.vmdk  - streamOptimized VMDK (the only subformat
//	               blessed by the OVF disk-format URI)
//
// The intermediate VMDK is written to a temp dir alongside the
// final .ova (NOT under /tmp) and removed after the tar
// finishes. /tmp is tmpfs on most Linux distros and a multi-GB
// streamOptimized VMDK exhausts the typical 16 GB tmpfs in
// minutes; the bundle dir lives on the operator's chosen
// output disk where space matches the .ova size.
func writeOVA(ctx context.Context, qcow2Src, ovaPath string, cfg Config) error {
	tmpDir, err := os.MkdirTemp(filepath.Dir(ovaPath), ".yc-ova-")
	if err != nil {
		return fmt.Errorf("ova tmp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	vmdkPath := filepath.Join(tmpDir, cfg.Name+".vmdk")
	convert := exec.CommandContext(ctx, "qemu-img", "convert",
		"-f", "qcow2", "-O", "vmdk",
		"-o", "subformat=streamOptimized",
		qcow2Src, vmdkPath,
	)
	if out, err := convert.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert (ova): %s: %w", out, err)
	}

	capacity, err := qemuImgVirtualSize(ctx, qcow2Src)
	if err != nil {
		return err
	}

	vmdkInfo, err := os.Stat(vmdkPath)
	if err != nil {
		return fmt.Errorf("stat ova vmdk: %w", err)
	}

	ovfBytes := []byte(renderOVF(cfg, capacity, vmdkInfo.Size()))
	ovfPath := filepath.Join(tmpDir, cfg.Name+".ovf")
	if err := os.WriteFile(ovfPath, ovfBytes, 0o644); err != nil {
		return fmt.Errorf("write ovf: %w", err)
	}

	out, err := os.Create(ovaPath)
	if err != nil {
		return fmt.Errorf("create ova: %w", err)
	}
	defer out.Close()

	tw := tar.NewWriter(out)
	// Order matters: OVF MUST come first so streaming OVA
	// readers can parse the descriptor before the disk bytes
	// land. Hence we write the .ovf entry, then the .vmdk.
	for _, name := range []string{cfg.Name + ".ovf", cfg.Name + ".vmdk"} {
		if err := tarAppendFile(tw, filepath.Join(tmpDir, name), name); err != nil {
			_ = tw.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close ova tar: %w", err)
	}
	return nil
}

// tarAppendFile streams srcPath into tw under archiveName.
func tarAppendFile(tw *tar.Writer, srcPath, archiveName string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	hdr := &tar.Header{
		Name:    archiveName,
		Mode:    0o644,
		Size:    st.Size(),
		ModTime: st.ModTime(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header for %s: %w", archiveName, err)
	}
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("tar copy %s: %w", archiveName, err)
	}
	return nil
}

// qemuImgVirtualSize returns the qcow2's virtual disk size in
// bytes, used as ovf:capacity in the OVF descriptor. We use the
// SOURCE qcow2's virtual size rather than re-statting the
// streamOptimized vmdk because qemu-img preserves virtual
// geometry across the convert and the qcow2 is what we already
// trust. ovf:capacity is the size guests see, NOT the on-disk
// file size; those are only the same for raw / monolithicFlat.
func qemuImgVirtualSize(ctx context.Context, qcow2Src string) (int64, error) {
	cmd := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", qcow2Src)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("qemu-img info: %w", err)
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return 0, fmt.Errorf("parse qemu-img info: %w", err)
	}
	if info.VirtualSize == 0 {
		return 0, fmt.Errorf("qemu-img info reported zero virtual-size for %s", qcow2Src)
	}
	return info.VirtualSize, nil
}

// renderOVF returns a minimal OVF 1.0 descriptor that VirtualBox
// and VMware Workstation accept. CPU + memory are taken from
// cfg; capacity is the qcow2's virtual disk size in bytes;
// vmdkBytes is the on-disk size of the streamOptimized VMDK
// shipped in the same tar (referenced from <References><File>).
//
// VirtualSystemType=virtualbox-2.2 makes VirtualBox happy
// without locking VMware out -- VMware ignores the value and
// applies its own defaults during import.
func renderOVF(cfg Config, capacity, vmdkBytes int64) string {
	name := html.EscapeString(cfg.Name)
	cpus := html.EscapeString(cfg.CPUs)
	mem := html.EscapeString(cfg.Memory)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1"
          xmlns:cim="http://schemas.dmtf.org/wbem/wscim/1/common"
          xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1"
          xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData"
          xmlns:vssd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_VirtualSystemSettingData"
          xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <References>
    <File ovf:href="%s.vmdk" ovf:id="file1" ovf:size="%d"/>
  </References>
  <DiskSection>
    <Info>List of the virtual disks</Info>
    <Disk ovf:capacity="%d" ovf:capacityAllocationUnits="byte" ovf:diskId="vmdisk1" ovf:fileRef="file1" ovf:format="http://www.vmware.com/interfaces/specifications/vmdk.html#streamOptimized"/>
  </DiskSection>
  <NetworkSection>
    <Info>The list of logical networks</Info>
    <Network ovf:name="NAT">
      <Description>NAT network</Description>
    </Network>
  </NetworkSection>
  <VirtualSystem ovf:id="%s">
    <Info>A y-cluster appliance</Info>
    <Name>%s</Name>
    <OperatingSystemSection ovf:id="94" ovf:version="22.04">
      <Info>The kind of installed guest operating system</Info>
      <Description>Ubuntu Linux (64-bit)</Description>
    </OperatingSystemSection>
    <VirtualHardwareSection>
      <Info>Virtual hardware requirements</Info>
      <System>
        <vssd:ElementName>Virtual Hardware Family</vssd:ElementName>
        <vssd:InstanceID>0</vssd:InstanceID>
        <vssd:VirtualSystemIdentifier>%s</vssd:VirtualSystemIdentifier>
        <vssd:VirtualSystemType>virtualbox-2.2</vssd:VirtualSystemType>
      </System>
      <Item>
        <rasd:Caption>%s virtual CPU</rasd:Caption>
        <rasd:Description>Number of virtual CPUs</rasd:Description>
        <rasd:ElementName>%s virtual CPU</rasd:ElementName>
        <rasd:InstanceID>1</rasd:InstanceID>
        <rasd:ResourceType>3</rasd:ResourceType>
        <rasd:VirtualQuantity>%s</rasd:VirtualQuantity>
      </Item>
      <Item>
        <rasd:AllocationUnits>MegaBytes</rasd:AllocationUnits>
        <rasd:Caption>%s MB of memory</rasd:Caption>
        <rasd:Description>Memory Size</rasd:Description>
        <rasd:ElementName>%s MB of memory</rasd:ElementName>
        <rasd:InstanceID>2</rasd:InstanceID>
        <rasd:ResourceType>4</rasd:ResourceType>
        <rasd:VirtualQuantity>%s</rasd:VirtualQuantity>
      </Item>
      <Item>
        <rasd:Address>0</rasd:Address>
        <rasd:Caption>sataController0</rasd:Caption>
        <rasd:Description>SATA Controller</rasd:Description>
        <rasd:ElementName>sataController0</rasd:ElementName>
        <rasd:InstanceID>3</rasd:InstanceID>
        <rasd:ResourceSubType>AHCI</rasd:ResourceSubType>
        <rasd:ResourceType>20</rasd:ResourceType>
      </Item>
      <Item>
        <rasd:AddressOnParent>0</rasd:AddressOnParent>
        <rasd:Caption>disk1</rasd:Caption>
        <rasd:Description>Disk Image</rasd:Description>
        <rasd:ElementName>disk1</rasd:ElementName>
        <rasd:HostResource>/disk/vmdisk1</rasd:HostResource>
        <rasd:InstanceID>4</rasd:InstanceID>
        <rasd:Parent>3</rasd:Parent>
        <rasd:ResourceType>17</rasd:ResourceType>
      </Item>
      <Item>
        <rasd:AutomaticAllocation>true</rasd:AutomaticAllocation>
        <rasd:Caption>Ethernet adapter on 'NAT'</rasd:Caption>
        <rasd:Connection>NAT</rasd:Connection>
        <rasd:ElementName>Ethernet adapter on 'NAT'</rasd:ElementName>
        <rasd:InstanceID>5</rasd:InstanceID>
        <rasd:ResourceSubType>E1000</rasd:ResourceSubType>
        <rasd:ResourceType>10</rasd:ResourceType>
      </Item>
    </VirtualHardwareSection>
  </VirtualSystem>
</Envelope>
`,
		name, vmdkBytes,
		capacity,
		name, name, name,
		cpus, cpus, cpus,
		mem, mem, mem,
	)
}
