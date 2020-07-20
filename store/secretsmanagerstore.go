package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/secretsmanager"
	"github.com/aws/aws-sdk-go/service/secretsmanager/secretsmanageriface"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/aws/aws-sdk-go/service/sts/stsiface"
)

// We store all Chamber metadata in a stringified JSON format,
// in a field named "_chamber_metadata"
const metadataKey = "_chamber_metadata"

// secretValueObject is the serialized format for storing secrets
// as a SecretsManager SecretValue
type secretValueObject map[string]string

// secretValueObjectMetadata holds all the metadata for all the secrets
// keyed by the name of the secret
type secretValueObjectMetadata map[string]secretMetadata

// secretMetadata holds all the metadata for a specific version
// of a specific secret
type secretMetadata struct {
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by"`
	Version   int       `json:"version"`
}

// ensure SecretsManagerStore confirms to Store interface
var _ Store = &SecretsManagerStore{}

// SecretsManagerStore implements the Store interface for storing secrets in SSM Parameter
// Store
type SecretsManagerStore struct {
	svc    secretsmanageriface.SecretsManagerAPI
	stsSvc stsiface.STSAPI
}

// NewSecretsManagerStore creates a new SecretsManagerStore
func NewSecretsManagerStore(numRetries int) (*SecretsManagerStore, error) {
	session, region, err := getSession(numRetries)
	if err != nil {
		return nil, err
	}

	svc := secretsmanager.New(session, &aws.Config{
		MaxRetries: aws.Int(numRetries),
		Region:     region,
	})

	stsSvc := sts.New(session, &aws.Config{
		MaxRetries: aws.Int(numRetries),
		Region:     region,
	})

	return &SecretsManagerStore{
		svc:    svc,
		stsSvc: stsSvc,
	}, nil
}

// Write writes a given value to a secret identified by id.  If the secret
// already exists, then write a new version.
func (s *SecretsManagerStore) Write(id SecretId, value string) error {
	version := 1
	// first read to get the current version
	latest, err := s.readLatest(id.Service)
	mustCreate := false

	if err != nil {
		if len(value) == 0 {
			return err
		}
		if err != ErrSecretNotFound {
			if awsErr, ok := err.(awserr.Error); ok {
				if awsErr.Code() == secretsmanager.ErrCodeResourceNotFoundException {
					mustCreate = true
				} else {
					return err
				}
			} else {
				return err
			}
		}
	}

	if len(value) == 0 {
		if mustCreate {
			return err
		}
		if _, ok := latest[id.Key]; ok {
			delete(latest, id.Key)
		} else {
			return ErrSecretNotFound
		}
		metadata, err := getHydratedMetadata(&latest)
		if err != nil {
			return err
		}
		if _, ok := metadata[id.Key]; ok {
			delete(metadata, id.Key)
		}

		rawMetadata, err := dehydrateMetadata(&metadata)
		if err != nil {
			return err
		}
		latest[metadataKey] = rawMetadata
	} else {
		user, err := s.getCurrentUser()
		if err != nil {
			return err
		}

		metadata, err := getHydratedMetadata(&latest)
		if err != nil {
			return err
		}

		if keyMetadata, ok := metadata[id.Key]; ok {
			version = keyMetadata.Version + 1
		}

		metadata[id.Key] = secretMetadata{
			Version:   version,
			Created:   time.Now().UTC(),
			CreatedBy: user,
		}

		rawMetadata, err := dehydrateMetadata(&metadata)
		if err != nil {
			return err
		}

		latest[id.Key] = value
		latest[metadataKey] = rawMetadata
	}

	contents, err := json.Marshal(latest)
	if err != nil {
		return err
	}

	if mustCreate {
		createSecretValueInput := &secretsmanager.CreateSecretInput{
			Name:         aws.String(id.Service),
			SecretString: aws.String(string(contents)),
		}
		_, err = s.svc.CreateSecret(createSecretValueInput)
		if err != nil {
			return err
		}
	} else {
		putSecretValueInput := &secretsmanager.PutSecretValueInput{
			SecretId:      aws.String(id.Service),
			SecretString:  aws.String(string(contents)),
			VersionStages: []*string{aws.String("AWSCURRENT"), aws.String("CHAMBER" + string(version))},
		}
		_, err = s.svc.PutSecretValue(putSecretValueInput)
		if err != nil {
			return err
		}
	}

	return nil
}

// Read reads a secret at a specific version.
// To grab the latest version, use -1 as the version number.
func (s *SecretsManagerStore) Read(id SecretId, version int) (Secret, error) {
	if version == -1 {
		latest, err := s.readLatest(id.Service)
		if err != nil {
			return Secret{}, err
		}

		value, ok := latest[id.Key]
		if !ok {
			return Secret{}, ErrSecretNotFound
		}

		keyMetadata, err := getHydratedKeyMetadata(&latest, &id.Key)
		if err != nil {
			return Secret{}, err
		}

		return Secret{
			Value: &value,
			Meta: SecretMetadata{
				Created:   keyMetadata.Created,
				CreatedBy: keyMetadata.CreatedBy,
				Version:   keyMetadata.Version,
				Key:       id.Key,
			},
		}, nil

	}
	return s.readVersion(id, version)
}

// Delete removes a secret. Note this removes all versions of the secret. (True?)
func (s *SecretsManagerStore) Delete(id SecretId) error {
	// delegate to Write
	return s.Write(id, "")
}

func (s *SecretsManagerStore) readVersion(id SecretId, version int) (Secret, error) {
	listSecretVersionIdsInput := &secretsmanager.ListSecretVersionIdsInput{
		SecretId:          aws.String(id.Service),
		IncludeDeprecated: aws.Bool(false),
	}

	var result Secret
	resp, err := s.svc.ListSecretVersionIds(listSecretVersionIdsInput)
	if err != nil {
		return Secret{}, err
	}

	for _, history := range resp.Versions {
		h := history
		thisVersion := 0

		getSecretValueInput := &secretsmanager.GetSecretValueInput{
			SecretId:  aws.String(id.Service),
			VersionId: h.VersionId,
		}

		resp, err := s.svc.GetSecretValue(getSecretValueInput)

		if err != nil {
			return Secret{}, err
		}

		if len(*resp.SecretString) == 0 {
			continue
		}

		var historyItem secretValueObject
		if historyItem, err = jsonToSecretValueObject(*resp.SecretString); err != nil {
			return Secret{}, err
		}

		keyMetadata, err := getHydratedKeyMetadata(&historyItem, &id.Key)
		if err != nil {
			return Secret{}, err
		}

		thisVersion = keyMetadata.Version

		if thisVersion == version {
			thisValue, ok := historyItem[id.Key]
			if !ok {
				return Secret{}, ErrSecretNotFound
			}
			result = Secret{
				Value: &thisValue,
				Meta: SecretMetadata{
					Created:   keyMetadata.Created,
					CreatedBy: keyMetadata.CreatedBy,
					Version:   thisVersion,
					Key:       id.Key,
				},
			}
			break
		}
	}

	if result.Value != nil {
		return result, nil
	}

	return Secret{}, ErrSecretNotFound
}

func (s *SecretsManagerStore) readLatest(service string) (secretValueObject, error) {
	getSecretValueInput := &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(service),
	}

	resp, err := s.svc.GetSecretValue(getSecretValueInput)

	if err != nil {
		return secretValueObject{}, err
	}

	if len(*resp.SecretString) == 0 {
		return secretValueObject{}, ErrSecretNotFound
	}

	var obj secretValueObject
	if obj, err = jsonToSecretValueObject(*resp.SecretString); err != nil {
		return secretValueObject{}, err
	}

	return obj, nil
}

// ListServices (not implemented)
func (s *SecretsManagerStore) ListServices(service string, includeSecretName bool) ([]string, error) {
	return nil, fmt.Errorf("Secrets Manager Backend is experimental and does not implement this command")
}

// List lists all secrets for a given service.  If includeValues is true,
// then those secrets are decrypted and returned, otherwise only the metadata
// about a secret is returned.
func (s *SecretsManagerStore) List(serviceName string, includeValues bool) ([]Secret, error) {
	secrets := map[string]Secret{}

	latest, err := s.readLatest(serviceName)
	if err != nil {
		return nil, err
	}

	metadata, err := getHydratedMetadata(&latest)
	if err != nil {
		return nil, err
	}

	for key, value := range latest {
		if key == metadataKey {
			continue
		}

		keyMetadata, ok := metadata[key]
		if !ok {
			continue
		}

		secret := Secret{
			Value: nil,
			Meta: SecretMetadata{
				Created:   keyMetadata.Created,
				CreatedBy: keyMetadata.CreatedBy,
				Version:   keyMetadata.Version,
				Key:       key,
			},
		}
		if includeValues {
			v := value
			secret.Value = &v
		}
		secrets[key] = secret
	}

	return values(secrets), nil
}

// ListRaw lists all secrets keys and values for a given service. Does not include any
// other metadata. Suitable for use in production environments.
func (s *SecretsManagerStore) ListRaw(serviceName string) ([]RawSecret, error) {
	latest, err := s.readLatest(serviceName)
	if err != nil {
		return nil, err
	}

	rawSecrets := make([]RawSecret, len(latest))
	i := 0
	for key, value := range latest {
		// v := value
		rawSecrets[i] = RawSecret{
			Value: value,
			Key:   key,
		}
		i++
	}
	return rawSecrets, nil
}

// History returns a list of events that have occurred regarding the given
// secret.
func (s *SecretsManagerStore) History(id SecretId) ([]ChangeEvent, error) {
	events := []ChangeEvent{}

	listSecretVersionIdsInput := &secretsmanager.ListSecretVersionIdsInput{
		SecretId:          aws.String(id.Service),
		IncludeDeprecated: aws.Bool(false),
	}

	resp, err := s.svc.ListSecretVersionIds(listSecretVersionIdsInput)
	if err != nil {
		return events, err
	}

	// m is a temporary map to allow us to (1) deduplicate ChangeEvents, since
	// saving the secret only increments the Version of the Key being created or
	// modified, and (2) sort the ChangeEvents by Version
	m := make(map[int]*ChangeEvent)

	for _, history := range resp.Versions {
		h := history
		getSecretValueInput := &secretsmanager.GetSecretValueInput{
			SecretId:  aws.String(id.Service),
			VersionId: h.VersionId,
		}

		resp, err := s.svc.GetSecretValue(getSecretValueInput)

		if err != nil {
			return events, err
		}

		if len(*resp.SecretString) == 0 {
			continue
		}

		var historyItem secretValueObject
		if historyItem, err = jsonToSecretValueObject(*resp.SecretString); err != nil {
			return events, err
		}

		metadata, err := getHydratedMetadata(&historyItem)
		if err != nil {
			return nil, err
		}

		keyMetadata, ok := metadata[id.Key]
		if !ok {
			continue
		}

		thisVersion := keyMetadata.Version

		// This is where we deduplicate
		if _, ok := m[thisVersion]; !ok {
			m[thisVersion] = &ChangeEvent{
				Type:    getChangeType(thisVersion),
				Time:    keyMetadata.Created,
				User:    keyMetadata.CreatedBy,
				Version: thisVersion,
			}
		}
	}

	if len(m) == 0 {
		return events, ErrSecretNotFound
	}

	keys := make([]int, 0)
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		events = append(events, *m[k])
	}
	return events, nil
}

func (s *SecretsManagerStore) getCurrentUser() (string, error) {
	resp, err := s.stsSvc.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}

	return *resp.Arn, nil
}

func getHydratedMetadata(raw *secretValueObject) (secretValueObjectMetadata, error) {
	r := *raw
	rawMetadata, ok := r[metadataKey]
	if !ok {
		return secretValueObjectMetadata{}, nil
	}
	return rehydrateMetadata(&rawMetadata)
}

func getHydratedKeyMetadata(raw *secretValueObject, key *string) (secretMetadata, error) {
	metadata, err := getHydratedMetadata(raw)
	if err != nil {
		return secretMetadata{}, err
	}

	keyMetadata, ok := metadata[*key]
	if !ok {
		return secretMetadata{}, nil
	}
	return keyMetadata, nil
}

func rehydrateMetadata(rawMetadata *string) (secretValueObjectMetadata, error) {
	var metadata secretValueObjectMetadata
	err := json.Unmarshal([]byte(*rawMetadata), &metadata)
	if err != nil {
		return secretValueObjectMetadata{}, err
	}
	return metadata, nil
}

func dehydrateMetadata(metadata *secretValueObjectMetadata) (string, error) {
	rawMetadata, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	return string(rawMetadata), nil
}

func jsonToSecretValueObject(s string) (secretValueObject, error) {
	var obj secretValueObject
	err := json.Unmarshal([]byte(s), &obj)
	if err != nil {
		return secretValueObject{}, err
	}
	return obj, nil
}
