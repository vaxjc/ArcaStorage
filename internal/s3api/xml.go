package s3api

import "encoding/xml"

type ownerXMLType struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type listBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Xmlns   string   `xml:"xmlns,attr"`
	Owner   ownerXMLType
	Buckets struct {
		Bucket []bucketXML `xml:"Bucket"`
	} `xml:"Buckets"`
}

type bucketXML struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type locationXML struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	Xmlns   string   `xml:"xmlns,attr"`
	Region  string   `xml:",chardata"`
}

type contentXMLType struct {
	Key          string       `xml:"Key"`
	LastModified string       `xml:"LastModified"`
	ETag         string       `xml:"ETag"`
	Size         int64        `xml:"Size"`
	StorageClass string       `xml:"StorageClass"`
	Owner        ownerXMLType `xml:"Owner"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

type listV2Result struct {
	XMLName               xml.Name         `xml:"ListBucketResult"`
	Xmlns                 string           `xml:"xmlns,attr"`
	Name                  string           `xml:"Name"`
	Prefix                string           `xml:"Prefix"`
	Delimiter             string           `xml:"Delimiter,omitempty"`
	MaxKeys               int              `xml:"MaxKeys"`
	KeyCount              int              `xml:"KeyCount"`
	IsTruncated           bool             `xml:"IsTruncated"`
	StartAfter            string           `xml:"StartAfter,omitempty"`
	ContinuationToken     string           `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string           `xml:"NextContinuationToken,omitempty"`
	Contents              []contentXMLType `xml:"Contents"`
	CommonPrefixes        []commonPrefix   `xml:"CommonPrefixes"`
}

type listV1Result struct {
	XMLName        xml.Name         `xml:"ListBucketResult"`
	Xmlns          string           `xml:"xmlns,attr"`
	Name           string           `xml:"Name"`
	Prefix         string           `xml:"Prefix"`
	Marker         string           `xml:"Marker"`
	NextMarker     string           `xml:"NextMarker,omitempty"`
	Delimiter      string           `xml:"Delimiter,omitempty"`
	MaxKeys        int              `xml:"MaxKeys"`
	IsTruncated    bool             `xml:"IsTruncated"`
	Contents       []contentXMLType `xml:"Contents"`
	CommonPrefixes []commonPrefix   `xml:"CommonPrefixes"`
}

type deleteRequest struct {
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
	Quiet bool `xml:"Quiet"`
}

type deleteResult struct {
	XMLName xml.Name      `xml:"DeleteResult"`
	Xmlns   string        `xml:"xmlns,attr"`
	Deleted []deletedKey  `xml:"Deleted"`
	Errors  []deleteError `xml:"Error"`
}

type deletedKey struct {
	Key string `xml:"Key"`
}

type deleteError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

type copyResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	Xmlns        string   `xml:"xmlns,attr"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

type initiateResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type completeRequest struct {
	Parts []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	} `xml:"Part"`
}

type completeResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type listPartsResult struct {
	XMLName     xml.Name     `xml:"ListPartsResult"`
	Xmlns       string       `xml:"xmlns,attr"`
	Bucket      string       `xml:"Bucket"`
	Key         string       `xml:"Key"`
	UploadID    string       `xml:"UploadId"`
	MaxParts    int          `xml:"MaxParts"`
	IsTruncated bool         `xml:"IsTruncated"`
	Owner       ownerXMLType `xml:"Owner"`
	Part        []partXML    `xml:"Part"`
}

type partXML struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

type taggingXML struct {
	XMLName xml.Name `xml:"Tagging"`
	Xmlns   string   `xml:"xmlns,attr"`
	TagSet  struct {
		Tag []tagXML `xml:"Tag"`
	} `xml:"TagSet"`
}

type tagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}
