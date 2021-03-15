package object_patch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/hashicorp/go-multierror"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/flant/shell-operator/pkg/app"
	"github.com/flant/shell-operator/pkg/jq"
	"github.com/flant/shell-operator/pkg/kube"
)

type ObjectPatcher struct {
	kubeClient kube.KubernetesClient
}

func NewObjectPatcher(kubeClient kube.KubernetesClient) *ObjectPatcher {
	return &ObjectPatcher{kubeClient: kubeClient}
}

func ParseSpecs(specBytes []byte) ([]OperationSpec, error) {
	specs, err := unmarshalFromJSONOrYAML(specBytes)
	if err != nil {
		return nil, err
	}

	var validationErrors = &multierror.Error{}
	for _, spec := range specs {
		err = ValidateOperationSpec(spec, GetSchema("v0"), "")
		if err != nil {
			validationErrors = multierror.Append(validationErrors, err)
		}
	}

	return specs, validationErrors.ErrorOrNil()
}

func (o *ObjectPatcher) GenerateFromJSONAndExecuteOperations(specs []OperationSpec) error {
	var applyErrors = &multierror.Error{}
	for _, spec := range specs {
		var operationError error

		switch spec.Operation {
		case Create:
			operationError = o.CreateObject(&unstructured.Unstructured{Object: spec.Object}, spec.Subresource)
		case CreateOrUpdate:
			operationError = o.CreateOrUpdateObject(&unstructured.Unstructured{Object: spec.Object}, spec.Subresource)
		case Delete:
			operationError = o.DeleteObject(spec.ApiVersion, spec.Kind, spec.Namespace, spec.Name, spec.Subresource)
		case DeleteInBackground:
			operationError = o.DeleteObjectInBackground(spec.ApiVersion, spec.Kind, spec.Namespace, spec.Name, spec.Subresource)
		case DeleteNonCascading:
			operationError = o.DeleteObjectNonCascading(spec.ApiVersion, spec.Kind, spec.Namespace, spec.Name, spec.Subresource)
		case JQPatch:
			operationError = o.JQPatchObject(spec.JQFilter, spec.ApiVersion, spec.Kind, spec.Namespace, spec.Name, spec.Subresource)
		case MergePatch:
			jsonMergePatch, err := json.Marshal(spec.MergePatch)
			if err != nil {
				applyErrors = multierror.Append(applyErrors, err)
				continue
			}

			operationError = o.MergePatchObject(jsonMergePatch, spec.ApiVersion, spec.Kind, spec.Namespace, spec.Name, spec.Subresource)
		case JSONPatch:
			jsonJsonPatch, err := json.Marshal(spec.JSONPatch)
			if err != nil {
				applyErrors = multierror.Append(applyErrors, err)
				continue
			}

			operationError = o.JSONPatchObject(jsonJsonPatch, spec.ApiVersion, spec.Kind, spec.Namespace, spec.Name, spec.Subresource)
		}

		if operationError != nil {
			applyErrors = multierror.Append(applyErrors, operationError)
		}
	}

	return applyErrors.ErrorOrNil()
}

func unmarshalFromJSONOrYAML(specs []byte) ([]OperationSpec, error) {
	fromJsonSpecs, err := unmarshalFromJson(specs)
	if err != nil {
		return unmarshalFromYaml(specs)
	}

	return fromJsonSpecs, nil
}

func unmarshalFromJson(jsonSpecs []byte) ([]OperationSpec, error) {
	var specSlice []OperationSpec

	dec := json.NewDecoder(bytes.NewReader(jsonSpecs))
	for {
		var doc OperationSpec
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		specSlice = append(specSlice, doc)
	}

	return specSlice, nil
}

func unmarshalFromYaml(yamlSpecs []byte) ([]OperationSpec, error) {
	var specSlice []OperationSpec

	dec := yaml.NewDecoder(bytes.NewReader(yamlSpecs))
	for {
		var doc OperationSpec
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		specSlice = append(specSlice, doc)
	}

	return specSlice, nil
}

func (o *ObjectPatcher) CreateObject(object *unstructured.Unstructured, subresource string) error {
	if object == nil {
		return fmt.Errorf("cannot create empty object")
	}

	apiVersion := object.GetAPIVersion()
	kind := object.GetKind()

	gvk, err := o.kubeClient.GroupVersionResource(apiVersion, kind)
	if err != nil {
		return err
	}

	_, err = o.kubeClient.Dynamic().Resource(gvk).Namespace(object.GetNamespace()).Create(context.TODO(), object, metav1.CreateOptions{}, generateSubresources(subresource)...)

	return err
}

func (o *ObjectPatcher) CreateOrUpdateObject(object *unstructured.Unstructured, subresource string) error {
	if object == nil {
		return fmt.Errorf("cannot create empty object")
	}

	apiVersion := object.GetAPIVersion()
	kind := object.GetKind()

	gvk, err := o.kubeClient.GroupVersionResource(apiVersion, kind)
	if err != nil {
		return err
	}

	_, err = o.kubeClient.Dynamic().Resource(gvk).Namespace(object.GetNamespace()).Create(context.TODO(), object, metav1.CreateOptions{}, generateSubresources(subresource)...)
	if errors.IsAlreadyExists(err) {
		err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			existingObj, err := o.kubeClient.Dynamic().Resource(gvk).Namespace(object.GetNamespace()).Get(context.TODO(), object.GetName(), metav1.GetOptions{}, generateSubresources(subresource)...)
			if err != nil {
				return err
			}

			objCopy := object.DeepCopy()
			objCopy.SetResourceVersion(existingObj.GetResourceVersion())
			_, err = o.kubeClient.Dynamic().Resource(gvk).Namespace(objCopy.GetNamespace()).Update(context.TODO(), objCopy, metav1.UpdateOptions{}, generateSubresources(subresource)...)
			return err
		})
	}

	return err
}

func (o *ObjectPatcher) FilterObject(filterFunc func(*unstructured.Unstructured) (*unstructured.Unstructured, error),
	apiVersion, kind, namespace, name, subresource string) error {

	if filterFunc == nil {
		return fmt.Errorf("FilterFunc is nil")
	}

	gvk, err := o.kubeClient.GroupVersionResource(apiVersion, kind)
	if err != nil {
		return err
	}

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		obj, err := o.kubeClient.Dynamic().Resource(gvk).Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		filteredObj, err := filterFunc(obj)
		if err != nil {
			return err
		}

		if equality.Semantic.DeepEqual(obj, filteredObj) {
			return nil
		}

		var filteredObjBuf bytes.Buffer
		err = unstructured.UnstructuredJSONScheme.Encode(filteredObj, &filteredObjBuf)
		if err != nil {
			return err
		}

		_, err = o.kubeClient.Dynamic().Resource(gvk).Namespace(namespace).Update(context.TODO(), filteredObj, metav1.UpdateOptions{}, generateSubresources(subresource)...)
		if err != nil {
			return err
		}

		return nil
	})

	return err
}

func (o *ObjectPatcher) JQPatchObject(jqPatch, apiVersion, kind, namespace, name, subresource string) error {
	gvk, err := o.kubeClient.GroupVersionResource(apiVersion, kind)
	if err != nil {
		return err
	}

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		obj, err := o.kubeClient.Dynamic().Resource(gvk).Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		patchedObj, err := applyJQPatch(jqPatch, obj)
		if err != nil {
			return err
		}

		_, err = o.kubeClient.Dynamic().Resource(gvk).Namespace(namespace).Update(context.TODO(), patchedObj, metav1.UpdateOptions{}, generateSubresources(subresource)...)
		if err != nil {
			return err
		}

		return nil
	})

	return err
}

func (o *ObjectPatcher) MergePatchObject(mergePatch []byte, apiVersion, kind, namespace, name, subresource string) error {
	gvk, err := o.kubeClient.GroupVersionResource(apiVersion, kind)
	if err != nil {
		return err
	}

	_, err = o.kubeClient.Dynamic().Resource(gvk).Namespace(namespace).Patch(context.TODO(), name, types.MergePatchType, mergePatch, metav1.PatchOptions{}, generateSubresources(subresource)...)

	return err
}

func (o *ObjectPatcher) JSONPatchObject(jsonPatch []byte, apiVersion, kind, namespace, name, subresource string) error {
	gvk, err := o.kubeClient.GroupVersionResource(apiVersion, kind)
	if err != nil {
		return err
	}

	_, err = o.kubeClient.Dynamic().Resource(gvk).Namespace(namespace).Patch(context.TODO(), name, types.JSONPatchType, jsonPatch, metav1.PatchOptions{}, generateSubresources(subresource)...)

	return err
}

func (o *ObjectPatcher) DeleteObject(apiVersion, kind, namespace, name, subresource string) error {
	return o.deleteObjectInternal(apiVersion, kind, namespace, name, subresource, metav1.DeletePropagationForeground)
}

func (o *ObjectPatcher) DeleteObjectInBackground(apiVersion, kind, namespace, name, subresource string) error {
	return o.deleteObjectInternal(apiVersion, kind, namespace, name, subresource, metav1.DeletePropagationBackground)
}

func (o *ObjectPatcher) DeleteObjectNonCascading(apiVersion, kind, namespace, name, subresource string) error {
	return o.deleteObjectInternal(apiVersion, kind, namespace, name, subresource, metav1.DeletePropagationOrphan)
}

func (o *ObjectPatcher) deleteObjectInternal(apiVersion, kind, namespace, name, subresource string, propagation metav1.DeletionPropagation) error {
	gvk, err := o.kubeClient.GroupVersionResource(apiVersion, kind)
	if err != nil {
		return err
	}

	err = o.kubeClient.Dynamic().Resource(gvk).Namespace(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{PropagationPolicy: &propagation}, subresource)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	if propagation != metav1.DeletePropagationForeground {
		return nil
	}

	err = wait.Poll(time.Second, 20*time.Second, func() (done bool, err error) {
		_, err = o.kubeClient.Dynamic().Resource(gvk).Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return true, nil
		}

		return false, err
	})

	return err
}

func applyJQPatch(jqFilter string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	objBytes, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}

	filterResult, err := jq.ApplyJqFilter(jqFilter, objBytes, app.JqLibraryPath)
	if err != nil {
		return nil, fmt.Errorf("failed to apply jqFilter:\n%sto Object:\n%s\n"+
			"error: %s", jqFilter, obj, err)
	}

	var retObj = &unstructured.Unstructured{}
	_, _, err = unstructured.UnstructuredJSONScheme.Decode([]byte(filterResult), nil, retObj)
	if err != nil {
		return nil, fmt.Errorf("failed to convert filterResult:\n%s\nto Unstructured Object\nerror: %s", filterResult, err)
	}

	return retObj, nil
}

func generateSubresources(subresource string) (ret []string) {
	if subresource != "" {
		ret = append(ret, subresource)
	}

	return
}
