import { EllipsisVertical, ShareIcon, UserPlusIcon } from "lucide-react";
import { type FC, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "react-query";
import { chatACL, setChatUserRole } from "#/api/queries/chats";
import type { ChatACLUser } from "#/api/typesGenerated";
import { AvatarData } from "#/components/Avatar/AvatarData";
import { Button } from "#/components/Button/Button";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuTrigger,
} from "#/components/DropdownMenu/DropdownMenu";
import { EmptyState } from "#/components/EmptyState/EmptyState";
import {
	Popover,
	PopoverContent,
	PopoverTrigger,
} from "#/components/Popover/Popover";
import { Spinner } from "#/components/Spinner/Spinner";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "#/components/Table/Table";
import { TableLoader } from "#/components/TableLoader/TableLoader";
import {
	UserOrGroupAutocomplete,
	type UserOrGroupAutocompleteValue,
} from "#/modules/workspaces/WorkspaceSharingForm/UserOrGroupAutocomplete";

interface ChatSharePopoverProps {
	chatId: string;
	organizationId: string;
}

export const ChatSharePopover: FC<ChatSharePopoverProps> = ({
	chatId,
	organizationId,
}) => {
	const queryClient = useQueryClient();
	const [selectedOption, setSelectedOption] =
		useState<UserOrGroupAutocompleteValue>(null);

	const aclQuery = useQuery(chatACL(chatId));
	const addUserMutation = useMutation(setChatUserRole(queryClient));
	const removeUserMutation = useMutation(setChatUserRole(queryClient));

	const users = aclQuery.data?.users ?? [];

	const handleAdd = () => {
		if (!selectedOption) return;
		addUserMutation.mutate(
			{ chatId, userId: selectedOption.id, role: "read" },
			{ onSuccess: () => setSelectedOption(null) },
		);
	};

	const handleRemove = (user: ChatACLUser) => {
		removeUserMutation.mutate({ chatId, userId: user.id, role: "" });
	};

	const tableHeader = (
		<TableHeader>
			<TableRow>
				<TableHead className="w-[50%] py-2">Member</TableHead>
				<TableHead className="w-[40%] py-2">Role</TableHead>
				<TableHead className="w-[10%] py-2" />
			</TableRow>
		</TableHeader>
	);

	const tableBody = (
		<TableBody>
			{aclQuery.isLoading ? (
				<TableLoader />
			) : users.length === 0 ? (
				<TableRow>
					<TableCell colSpan={999}>
						<EmptyState
							message="Not shared with anyone yet"
							description="Add a member using the search above."
							isCompact
						/>
					</TableCell>
				</TableRow>
			) : (
				users.map((user) => (
					<TableRow key={user.id}>
						<TableCell className="py-2 w-[50%]">
							<AvatarData
								title={user.username}
								subtitle={user.name}
								src={user.avatar_url}
							/>
						</TableCell>
						<TableCell className="py-2 w-[40%]">
							<span className="bg-surface-secondary rounded-md px-3 py-0.5 inline-block text-sm">
								Read only
							</span>
						</TableCell>
						<TableCell className="py-2 w-[10%]">
							<DropdownMenu>
								<DropdownMenuTrigger asChild>
									<Button
										size="icon-lg"
										variant="subtle"
										aria-label="Open menu"
									>
										<EllipsisVertical aria-hidden="true" />
										<span className="sr-only">Open menu</span>
									</Button>
								</DropdownMenuTrigger>
								<DropdownMenuContent align="end">
									<DropdownMenuItem
										className="text-content-destructive focus:text-content-destructive"
										onClick={() => handleRemove(user)}
									>
										Remove
									</DropdownMenuItem>
								</DropdownMenuContent>
							</DropdownMenu>
						</TableCell>
					</TableRow>
				))
			)}
		</TableBody>
	);

	return (
		<Popover>
			<PopoverTrigger asChild>
				<Button
					size="icon"
					variant="subtle"
					className="h-7 w-7 text-content-secondary hover:text-content-primary"
					aria-label="Share chat"
				>
					<ShareIcon className="h-4 w-4" />
				</Button>
			</PopoverTrigger>
			<PopoverContent align="end" className="w-[580px] p-4">
				<h3 className="text-lg font-semibold m-0 mb-1">Share Chat</h3>
				<p className="mb-4 text-sm text-content-secondary">
					Add users who can view this chat (read-only).
				</p>

				{addUserMutation.isError && (
					<p className="mb-2 text-xs text-content-destructive">
						Failed to add user. Please try again.
					</p>
				)}

				<form
					action={handleAdd}
					className="flex flex-row items-center gap-2 mb-4"
				>
					<UserOrGroupAutocomplete
						organizationId={organizationId}
						value={selectedOption}
						exclude={[...users]}
						onChange={(newValue) => setSelectedOption(newValue)}
					/>
					<Button
						disabled={!selectedOption || addUserMutation.isPending}
						type="submit"
					>
						<Spinner loading={addUserMutation.isPending}>
							<UserPlusIcon className="size-icon-sm" />
						</Spinner>
						Add member
					</Button>
				</form>

				<div>
					<Table>{tableHeader}</Table>
					<div className="max-h-60 overflow-y-auto">
						<Table>{tableBody}</Table>
					</div>
				</div>
			</PopoverContent>
		</Popover>
	);
};
