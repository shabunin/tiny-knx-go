package product

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"text/template"

	"github.com/shabunin/tiny-knx-go/device"
)

// Sizes of the group objects by DPT main number.
var objectSizes = map[string]string{
	"1":  "1 Bit",
	"3":  "4 Bit",
	"5":  "1 Byte",
	"9":  "2 Bytes",
	"14": "4 Bytes",
}

func mainType(dpt string) string {
	main, _, _ := strings.Cut(dpt, ".")
	return main
}

// WriteXML writes the product database. Convert it with OpenKNXproducer on a computer with ETS:
//
//	OpenKNXproducer knxprod -o product.knxprod product.xml
func (p *Product) WriteXML(w io.Writer) error {
	if err := p.Check(); err != nil {
		return err
	}

	// The template is parsed here, not when the package loads, so that firmware that only reads its
	// parameters does not link the template engine.
	t, err := template.New("database").Funcs(template.FuncMap{"x": escape}).Parse(databaseTemplate)
	if err != nil {
		return err
	}

	var buffer bytes.Buffer
	if err := t.Execute(&buffer, p.model()); err != nil {
		return err
	}

	_, err = w.Write(buffer.Bytes())

	return err
}

// The IDs follow the scheme of ETS: M-00FA_A-<application>-<version>-0000 for the application,
// with the parts below it appended; OpenKNXproducer replaces 0000 with a hash.
type model struct {
	*Product
	Hardware, HardwareID, ProductID, H2PID, AppID string
	Number, Version                               string
	ParamSize                                     int
	Params                                        []paramModel
	Objects                                       []objectModel
	Blocks                                        []blockModel
}

type paramModel struct {
	Param
	ID, TypeID, RefID string
}

type objectModel struct {
	Object
	Number        int
	ID, RefID     string
	Size, DPST    string
	R, W, C, T, U string
	I             string
}

type blockModel struct {
	ID      string
	Ident   string // Name attribute, without spaces
	Name    string
	Params  []paramModel
	Objects []objectModel
}

func (p *Product) model() *model {
	m := &model{
		Product:   p,
		Hardware:  fmt.Sprintf("%04X", p.Application),
		Number:    fmt.Sprintf("%04X", p.Application),
		Version:   fmt.Sprintf("%02X", p.Version),
		ParamSize: p.ParamSize(),
	}

	m.HardwareID = fmt.Sprintf("M-%04X_H-%s-1", Manufacturer, m.Hardware)
	m.ProductID = m.HardwareID + "_P-" + p.OrderNumber
	m.H2PID = fmt.Sprintf("%s_HP-%s-%s-0000", m.HardwareID, m.Number, m.Version)
	m.AppID = fmt.Sprintf("M-%04X_A-%s-%s-0000", Manufacturer, m.Number, m.Version)

	enabled := func(flags, flag device.Flags) string {
		if flags&flag != 0 {
			return "Enabled"
		}

		return "Disabled"
	}

	n := 0

	for i, b := range p.Blocks {
		block := blockModel{ID: fmt.Sprintf("%s_PB-%d", m.AppID, i+1), Ident: fmt.Sprintf("Page%d", i+1), Name: b.Name}

		for _, param := range b.Params {
			pm := paramModel{Param: param}
			pm.TypeID = m.AppID + "_PT-" + param.Name
			pm.ID = fmt.Sprintf("%s_P-%d", m.AppID, len(m.Params)+1)
			pm.RefID = fmt.Sprintf("%s_R-%d", pm.ID, len(m.Params)+1)

			m.Params = append(m.Params, pm)
			block.Params = append(block.Params, pm)
		}

		for _, o := range b.Objects {
			n++

			om := objectModel{Object: o, Number: n}
			om.ID = fmt.Sprintf("%s_O-%d", m.AppID, n)
			om.RefID = fmt.Sprintf("%s_R-%d", om.ID, n)
			om.Size = objectSizes[mainType(o.DPT)]
			om.DPST = dpst(o.DPT)
			om.C = enabled(o.Flags, device.FlagCommunicate)
			om.R = enabled(o.Flags, device.FlagRead)
			om.W = enabled(o.Flags, device.FlagWrite)
			om.T = enabled(o.Flags, device.FlagTransmit)
			om.U = enabled(o.Flags, device.FlagUpdate)
			om.I = enabled(o.Flags, device.FlagInit)

			m.Objects = append(m.Objects, om)
			block.Objects = append(block.Objects, om)
		}

		m.Blocks = append(m.Blocks, block)
	}

	return m
}

// dpst converts "9.001" to the datapoint subtype ID "DPST-9-1".
func dpst(dpt string) string {
	main, sub, _ := strings.Cut(dpt, ".")
	return "DPST-" + main + "-" + strings.TrimLeft(sub, "0")
}

func escape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))

	return b.String()
}

const databaseTemplate = `<?xml version="1.0" encoding="utf-8"?>
<KNX xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema" CreatedBy="tiny-knx-go" ToolVersion="1.0" xmlns="http://knx.org/xml/project/11">
  <ManufacturerData>
    <Manufacturer RefId="M-00FA">
      <Catalog>
        <CatalogSection Id="M-00FA_CS-1" Name="tiny-knx-go" Number="1" DefaultLanguage="en-US">
          <CatalogItem Id="{{.H2PID}}_CI-{{.OrderNumber}}-1" Name="{{x .Name}}" Number="1" ProductRefId="{{.ProductID}}" Hardware2ProgramRefId="{{.H2PID}}" DefaultLanguage="en-US" />
        </CatalogSection>
      </Catalog>
      <ApplicationPrograms>
        <ApplicationProgram Id="{{.AppID}}" ApplicationNumber="{{.Application}}" ApplicationVersion="{{.Product.Version}}" ProgramType="ApplicationProgram" MaskVersion="MV-07B0" Name="{{x .Name}}" LoadProcedureStyle="MergedProcedure" PeiType="0" DefaultLanguage="en-US" DynamicTableManagement="false" Linkable="false" MinEtsVersion="4.0">
          <Static>
            <Code>
              <RelativeSegment Id="{{.AppID}}_RS-04-00000" Name="Parameters" Offset="0" Size="{{.ParamSize}}" LoadStateMachine="4" />
            </Code>
            <ParameterTypes>
{{- range .Params}}
              <ParameterType Id="{{.TypeID}}" Name="{{x .Name}}">
{{- if .Values}}
                <TypeRestriction Base="Value" SizeInBit="{{.Bits}}">
{{- $type := .TypeID}}{{range .Values}}
                  <Enumeration Text="{{x .Text}}" Value="{{.Value}}" Id="{{$type}}_EN-{{.Value}}" />
{{- end}}
                </TypeRestriction>
{{- else}}
                <TypeNumber SizeInBit="{{.Bits}}" Type="unsignedInt" minInclusive="{{.Min}}" maxInclusive="{{.Max}}" />
{{- end}}
              </ParameterType>
{{- end}}
            </ParameterTypes>
            <Parameters>
{{- range .Params}}
              <Parameter Id="{{.ID}}" Name="{{x .Name}}" ParameterType="{{.TypeID}}" Text="{{x .Text}}" Value="{{.Default}}">
                <Memory CodeSegment="{{$.AppID}}_RS-04-00000" Offset="{{.Offset}}" BitOffset="0" />
              </Parameter>
{{- end}}
            </Parameters>
            <ParameterRefs>
{{- range .Params}}
              <ParameterRef Id="{{.RefID}}" RefId="{{.ID}}" />
{{- end}}
            </ParameterRefs>
            <ComObjectTable>
{{- range .Objects}}
              <ComObject Id="{{.ID}}" Name="{{x .Name}}" Text="{{x .Name}}" Number="{{.Number}}" FunctionText="{{x .Function}}" ObjectSize="{{.Size}}" ReadFlag="{{.R}}" WriteFlag="{{.W}}" CommunicationFlag="{{.C}}" TransmitFlag="{{.T}}" UpdateFlag="{{.U}}" ReadOnInitFlag="{{.I}}" DatapointType="{{.DPST}}" />
{{- end}}
            </ComObjectTable>
            <ComObjectRefs>
{{- range .Objects}}
              <ComObjectRef Id="{{.RefID}}" RefId="{{.ID}}" />
{{- end}}
            </ComObjectRefs>
            <AddressTable MaxEntries="65000" />
            <AssociationTable MaxEntries="65000" />
            <LoadProcedures>
              <LoadProcedure MergeId="2">
                <LdCtrlRelSegment AppliesTo="full" LsmIdx="4" Size="{{.ParamSize}}" Mode="0" Fill="0" />
              </LoadProcedure>
              <LoadProcedure MergeId="4">
                <LdCtrlWriteRelMem ObjIdx="4" Offset="0" Size="{{.ParamSize}}" Verify="true" />
              </LoadProcedure>
            </LoadProcedures>
            <Options />
          </Static>
          <Dynamic>
            <ChannelIndependentBlock>
{{- range .Blocks}}
              <ParameterBlock Id="{{.ID}}" Name="{{.Ident}}" Text="{{x .Name}}">
{{- range .Params}}
                <ParameterRefRef RefId="{{.RefID}}" />
{{- end}}
{{- range .Objects}}
                <ComObjectRefRef RefId="{{.RefID}}" />
{{- end}}
              </ParameterBlock>
{{- end}}
            </ChannelIndependentBlock>
          </Dynamic>
        </ApplicationProgram>
      </ApplicationPrograms>
      <Hardware>
        <Hardware Id="{{.HardwareID}}" Name="{{x .Name}}" SerialNumber="{{.Hardware}}" VersionNumber="1" BusCurrent="10" HasIndividualAddress="true" HasApplicationProgram="true">
          <Products>
            <Product Id="{{.ProductID}}" Text="{{x .Name}}" OrderNumber="{{.OrderNumber}}" IsRailMounted="false" DefaultLanguage="en-US">
              <RegistrationInfo RegistrationStatus="Registered" />
            </Product>
          </Products>
          <Hardware2Programs>
            <Hardware2Program Id="{{.H2PID}}" MediumTypes="MT-0">
              <ApplicationProgramRef RefId="{{.AppID}}" />
              <RegistrationInfo RegistrationStatus="Registered" RegistrationNumber="0001/1{{.Product.Version}}" />
            </Hardware2Program>
          </Hardware2Programs>
        </Hardware>
      </Hardware>
    </Manufacturer>
  </ManufacturerData>
</KNX>
`
