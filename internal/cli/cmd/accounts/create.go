/*
Copyright (C) 2022-2023 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package accounts

import (
	"github.com/sethvargo/go-password/password"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	lorryutil "github.com/apecloud/kubeblocks/lorry/util"
)

type CreateUserOptions struct {
	*AccountBaseOptions
	info lorryutil.UserInfo
}

func NewCreateUserOptions(f cmdutil.Factory, streams genericclioptions.IOStreams) *CreateUserOptions {
	return &CreateUserOptions{
		AccountBaseOptions: NewAccountBaseOptions(f, streams, lorryutil.CreateUserOp),
	}
}

func (o *CreateUserOptions) AddFlags(cmd *cobra.Command) {
	o.AccountBaseOptions.AddFlags(cmd)
	cmd.Flags().StringVar(&o.info.UserName, "name", "", "Required. Specify the name of user, which must be unique.")
	cmd.Flags().StringVarP(&o.info.Password, "password", "p", "", "Optional. Specify the password of user. The default value is empty, which means a random password will be generated.")
	_ = cmd.MarkFlagRequired("name")
	// TODO:@shanshan add expire flag if needed
	// cmd.Flags().DurationVar(&o.info.ExpireAt, "expire", 0, "Optional. Specify the expired time of password. The default value is 0, which means the user will never expire.")
}

func (o CreateUserOptions) Validate(args []string) error {
	if err := o.AccountBaseOptions.Validate(args); err != nil {
		return err
	}
	if len(o.info.UserName) == 0 {
		return errMissingUserName
	}
	return nil
}

func (o *CreateUserOptions) Complete(f cmdutil.Factory) error {
	var err error
	if err = o.AccountBaseOptions.Complete(f); err != nil {
		return err
	}
	// complete other options
	if len(o.info.Password) == 0 {
		o.info.Password, _ = password.Generate(10, 2, 0, false, false)
	}
	// encode user info to metadata
	o.RequestMeta, err = struct2Map(o.info)
	return err
}
